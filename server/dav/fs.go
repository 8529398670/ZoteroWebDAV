package dav

import (
	context "context"
	errors "errors"
	log "log"
	os "os"
	path "path"
	filepath "path/filepath"
	regexp "regexp"
	strings "strings"

	webdav "golang.org/x/net/webdav"

	models "zoterowebdav/server/models"
)

// reservedPrefix marks the files this server keeps for itself inside a
// folder (uploads in progress). A client can neither see nor name them.
const reservedPrefix = ".zwd-"

// zoteroFile is a Zotero attachment's file in the folder Zotero syncs to.
var zoteroFile = regexp.MustCompile( `^/zotero/([A-Z0-9]{8})\.(prop|zip)$` )

// userFS is one account's folder as x/net/webdav sees it.
//
// What it adds to a plain directory:
//
//   - nothing outside the folder can be reached: the path is cleaned, and
//     no symbolic link is followed anywhere along it;
//   - a write goes to a hidden file beside the target and replaces it only
//     once complete (file.go), so a reader never sees half a file and a
//     failed upload leaves the old one;
//   - the quota and the usage count;
//   - the attachment index, kept as zotero/<KEY>.prop and .zip come and go.
type userFS struct {
	server *Server
	userID uint64
	root   string
	state  *userState
}

var _ webdav.FileSystem = ( *userFS )( nil )

// resolve turns a WebDAV path into the file it names, or refuses it.
func ( fs *userFS ) resolve( name string ) ( full string , rel string , err error ) {
	if strings.ContainsRune( name , 0 ) {
		err = os.ErrNotExist
		return
	}
	rel = path.Clean( "/" + name )
	for _ , part := range strings.Split( rel , "/" ) {
		if strings.HasPrefix( part , reservedPrefix ) {
			err = os.ErrNotExist
			return
		}
	}
	full = filepath.Join( fs.root , filepath.FromSlash( rel ) )
	err = noLinks( fs.root , rel )
	return
}

// noLinks refuses a path with a symbolic link, or anything but a folder or a
// plain file, on the way. Nothing here makes links; this is for one put
// there by hand.
func noLinks( root string , rel string ) ( err error ) {
	current := root
	for _ , part := range strings.Split( strings.Trim( rel , "/" ) , "/" ) {
		if part == "" { continue }
		current = filepath.Join( current , part )
		info , stat_err := os.Lstat( current )
		if errors.Is( stat_err , os.ErrNotExist ) { return }
		if stat_err != nil {
			err = stat_err
			return
		}
		if info.Mode().IsDir() == false && info.Mode().IsRegular() == false {
			err = os.ErrNotExist
			return
		}
	}
	return
}

func ( fs *userFS ) Mkdir( ctx context.Context , name string , perm os.FileMode ) ( err error ) {
	full , rel , err := fs.resolve( name )
	if err != nil { return }
	if rel == "/" {
		err = os.ErrExist
		return
	}
	err = os.Mkdir( full , 0o700 )
	return
}

const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_TRUNC | os.O_APPEND

func ( fs *userFS ) OpenFile( ctx context.Context , name string , flag int , perm os.FileMode ) ( file webdav.File , err error ) {
	full , rel , err := fs.resolve( name )
	if err != nil { return }
	if flag & writeFlags == 0 {
		file , err = fs.server.openRead( full , rel )
		return
	}
	// A write replaces a whole file: x/net/webdav only ever asks for that
	// (PUT and COPY), and a partial overwrite cannot be made atomic.
	if rel == "/" || flag & os.O_APPEND != 0 || flag & os.O_TRUNC == 0 && flag & os.O_EXCL == 0 {
		err = os.ErrPermission
		return
	}
	parent , stat_err := os.Lstat( filepath.Dir( full ) )
	if stat_err != nil || parent.IsDir() == false {
		err = os.ErrNotExist
		return
	}
	existing , stat_err := os.Lstat( full )
	switch {
	case stat_err == nil && existing.IsDir():
		err = os.ErrExist
		return
	case stat_err == nil && flag & os.O_EXCL != 0:
		err = os.ErrExist
		return
	}
	put , _ := ctx.Value( putStateKey ).( *putState )
	file , err = fs.newWriteFile( full , rel , put )
	return
}

func ( fs *userFS ) RemoveAll( ctx context.Context , name string ) ( err error ) {
	full , rel , err := fs.resolve( name )
	if err != nil { return }
	if rel == "/" {
		err = os.ErrPermission
		return
	}
	info , err := os.Lstat( full )
	if err != nil { return }
	size := treeSize( full )
	if err = os.RemoveAll( full ); err != nil { return }
	fs.server.addUsage( fs.userID , fs.state , -size )
	fs.noteRemoved( rel , info.IsDir() )
	return
}

func ( fs *userFS ) Rename( ctx context.Context , old_name string , new_name string ) ( err error ) {
	old_full , old_rel , err := fs.resolve( old_name )
	if err != nil { return }
	new_full , new_rel , err := fs.resolve( new_name )
	if err != nil { return }
	if old_rel == "/" || new_rel == "/" {
		err = os.ErrPermission
		return
	}
	info , err := os.Lstat( old_full )
	if err != nil { return }
	if err = os.Rename( old_full , new_full ); err != nil { return }
	fs.noteRemoved( old_rel , info.IsDir() )
	fs.noteMoved( new_rel , info.IsDir() )
	return
}

func ( fs *userFS ) Stat( ctx context.Context , name string ) ( info os.FileInfo , err error ) {
	full , _ , err := fs.resolve( name )
	if err != nil { return }
	info , err = os.Lstat( full )
	if err != nil { return }
	if info.Mode().IsDir() == false && info.Mode().IsRegular() == false {
		info , err = nil , os.ErrNotExist
		return
	}
	info = plainInfo( full , info )
	return
}

// touchesZotero says whether a folder rel holds, or is, zotero/.
func touchesZotero( rel string ) ( result bool ) {
	result = rel == "/" || rel == "/zotero" || strings.HasPrefix( rel , "/zotero/" )
	return
}

// noteWritten keeps the attachment index current after a file lands. head is
// the start of what was written, enough for a .prop.
func ( fs *userFS ) noteWritten( rel string , plain_size int64 , head []byte ) {
	match := zoteroFile.FindStringSubmatch( rel )
	if match == nil { return }
	var err error
	if match[ 2 ] == "prop" {
		err = models.NoteDAVProp( fs.server.Store , fs.userID , match[ 1 ] , head )
	} else {
		err = models.NoteDAVZip( fs.server.Store , fs.userID , match[ 1 ] , plain_size )
	}
	if err != nil { log.Printf( "[webdav] attachment index: %v" , err ) }
}

func ( fs *userFS ) noteRemoved( rel string , was_dir bool ) {
	if was_dir {
		if touchesZotero( rel ) { fs.reindex() }
		return
	}
	match := zoteroFile.FindStringSubmatch( rel )
	if match == nil { return }
	var err error
	if match[ 2 ] == "prop" {
		err = models.ForgetDAVProp( fs.server.Store , fs.userID , match[ 1 ] )
	} else {
		err = models.ForgetDAVZip( fs.server.Store , fs.userID , match[ 1 ] )
	}
	if err != nil { log.Printf( "[webdav] attachment index: %v" , err ) }
}

// noteMoved is noteWritten for something renamed into place.
func ( fs *userFS ) noteMoved( rel string , is_dir bool ) {
	if is_dir {
		if touchesZotero( rel ) { fs.reindex() }
		return
	}
	if zoteroFile.MatchString( rel ) { fs.reindex() }
}

func ( fs *userFS ) reindex() {
	if err := fs.server.reindex( fs.userID ); err != nil {
		log.Printf( "[webdav] attachment index of account %d: %v" , fs.userID , err )
	}
}

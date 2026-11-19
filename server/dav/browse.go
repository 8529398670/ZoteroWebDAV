package dav

import (
	json "encoding/json"
	errors "errors"
	io "io"
	os "os"
	path "path"
	filepath "path/filepath"
	regexp "regexp"
	sort "sort"
	time "time"

	encryption "zoterowebdav/server/encryption"
)

// The WebDAV screen's view of an account's folder: list, download, delete
// into a trash, and put back. It goes through the same userFS as a WebDAV
// client, so the same paths are refused and the same counts kept.

var (
	ErrNotFolder  = errors.New( "not a folder" )
	ErrNotFile    = errors.New( "not a file" )
	ErrIsRoot     = errors.New( "the top folder cannot be deleted" )
	ErrTrashGone  = errors.New( "that is not in the trash" )
	ErrRestoreTaken = errors.New( "something else is at that path now -- move it first" )
)

// Entry is one thing in a folder.
type Entry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// TrashEntry is one thing in the trash.
type TrashEntry struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	DeletedAt time.Time `json:"deleted_at"`
}

var trashIDPattern = regexp.MustCompile( `^[A-Za-z0-9_-]{16,64}$` )

const (
	trashItem = "item"
	trashMeta = "meta.json"
)

func ( server *Server ) fsFor( user_id uint64 ) ( fs *userFS ) {
	state := server.state( user_id )
	fs = &userFS{ server: server , userID: user_id , root: server.Root( user_id ) , state: state }
	return
}

// List is what a folder holds, folders first, then by name.
func ( server *Server ) List( user_id uint64 , rel string ) ( entries []Entry , err error ) {
	fs := server.fsFor( user_id )
	if err = server.ensureRoot( user_id , fs.state ); err != nil { return }
	full , clean , err := fs.resolve( rel )
	if err != nil { return }
	file , err := server.openRead( full , clean )
	if err != nil { return }
	defer file.Close()
	if file.info.IsDir() == false {
		err = ErrNotFolder
		return
	}
	infos , err := file.Readdir( 0 )
	if err != nil { return }
	entries = make( []Entry , 0 , len( infos ) )
	for _ , info := range infos {
		entries = append( entries , Entry{ Name: info.Name() , Path: path.Join( clean , info.Name() ) , IsDir: info.IsDir() , Size: info.Size() , Modified: info.ModTime().UTC() } )
	}
	sort.Slice( entries , func( i int , j int ) bool {
		if entries[ i ].IsDir != entries[ j ].IsDir { return entries[ i ].IsDir }
		return entries[ i ].Name < entries[ j ].Name
	} )
	return
}

// File is a file opened for download.
type File interface {
	io.ReadSeekCloser
	io.ReaderAt
	Size() int64
}

// Open opens a file in an account's folder for reading, decrypted.
func ( server *Server ) Open( user_id uint64 , rel string ) ( file File , info os.FileInfo , err error ) {
	fs := server.fsFor( user_id )
	full , clean , err := fs.resolve( rel )
	if err != nil { return }
	opened , err := server.openRead( full , clean )
	if err != nil { return }
	if opened.info.Mode().IsRegular() == false {
		opened.Close()
		err = ErrNotFile
		return
	}
	file , info = opened , opened.info
	return
}

// Trash moves a file or folder into the account's trash. The room it takes
// still counts until the trash is emptied.
func ( server *Server ) Trash( user_id uint64 , rel string ) ( entry TrashEntry , err error ) {
	fs := server.fsFor( user_id )
	full , clean , err := fs.resolve( rel )
	if err != nil { return }
	if clean == "/" {
		err = ErrIsRoot
		return
	}
	info , err := os.Lstat( full )
	if err != nil { return }
	entry = TrashEntry{ ID: encryption.GenerateURLToken( 16 ) , Path: clean , IsDir: info.IsDir() , Size: treeSize( full ) , DeletedAt: time.Now().UTC() }
	folder := filepath.Join( server.TrashRoot( user_id ) , entry.ID )
	if err = os.MkdirAll( folder , 0o700 ); err != nil { return }
	meta , err := json.Marshal( entry )
	if err == nil { err = os.WriteFile( filepath.Join( folder , trashMeta ) , meta , 0o600 ) }
	if err == nil { err = os.Rename( full , filepath.Join( folder , trashItem ) ) }
	if err != nil {
		os.RemoveAll( folder )
		return
	}
	fs.noteRemoved( clean , entry.IsDir )
	return
}

// Restore puts a trash entry back where it was. A folder that was its parent
// is made again if it went too.
func ( server *Server ) Restore( user_id uint64 , trash_id string ) ( entry TrashEntry , err error ) {
	if trashIDPattern.MatchString( trash_id ) == false {
		err = ErrTrashGone
		return
	}
	folder := filepath.Join( server.TrashRoot( user_id ) , trash_id )
	raw , err := os.ReadFile( filepath.Join( folder , trashMeta ) )
	if err != nil {
		err = ErrTrashGone
		return
	}
	if err = json.Unmarshal( raw , &entry ); err != nil { return }
	fs := server.fsFor( user_id )
	full , clean , err := fs.resolve( entry.Path )
	if err != nil { return }
	if _ , stat_err := os.Lstat( full ); stat_err == nil {
		err = ErrRestoreTaken
		return
	}
	if err = noLinks( fs.root , path.Dir( clean ) ); err != nil { return }
	if err = os.MkdirAll( filepath.Dir( full ) , 0o700 ); err != nil { return }
	if err = os.Rename( filepath.Join( folder , trashItem ) , full ); err != nil { return }
	os.RemoveAll( folder )
	fs.noteMoved( clean , entry.IsDir )
	return
}

// ListTrash is the account's trash, newest first.
func ( server *Server ) ListTrash( user_id uint64 ) ( entries []TrashEntry , err error ) {
	entries = []TrashEntry{}
	folders , err := os.ReadDir( server.TrashRoot( user_id ) )
	if errors.Is( err , os.ErrNotExist ) {
		err = nil
		return
	}
	if err != nil { return }
	for _ , folder := range folders {
		raw , read_err := os.ReadFile( filepath.Join( server.TrashRoot( user_id ) , folder.Name() , trashMeta ) )
		if read_err != nil { continue }
		entry := TrashEntry{}
		if json.Unmarshal( raw , &entry ) == nil && entry.ID == folder.Name() {
			entries = append( entries , entry )
		}
	}
	sort.Slice( entries , func( i int , j int ) bool { return entries[ i ].DeletedAt.After( entries[ j ].DeletedAt ) } )
	return
}

// EmptyTrashEntry removes one trash entry for good.
func ( server *Server ) EmptyTrashEntry( user_id uint64 , trash_id string ) ( err error ) {
	if trashIDPattern.MatchString( trash_id ) == false {
		err = ErrTrashGone
		return
	}
	folder := filepath.Join( server.TrashRoot( user_id ) , trash_id )
	if _ , err = os.Stat( folder ); err != nil {
		err = ErrTrashGone
		return
	}
	size := treeSize( folder )
	if err = os.RemoveAll( folder ); err != nil { return }
	server.addUsage( user_id , server.state( user_id ) , -size )
	return
}

// purgeTrash removes trash entries older than age, and folders in the trash
// that are not entries at all.
func ( server *Server ) purgeTrash( user_id uint64 , age time.Duration ) ( removed int ) {
	folders , err := os.ReadDir( server.TrashRoot( user_id ) )
	if err != nil { return }
	cutoff := time.Now().Add( -age )
	for _ , folder := range folders {
		full := filepath.Join( server.TrashRoot( user_id ) , folder.Name() )
		raw , read_err := os.ReadFile( filepath.Join( full , trashMeta ) )
		entry := TrashEntry{}
		if read_err == nil && json.Unmarshal( raw , &entry ) == nil && entry.DeletedAt.After( cutoff ) { continue }
		if os.RemoveAll( full ) == nil { removed += 1 }
	}
	return
}

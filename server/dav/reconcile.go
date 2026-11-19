package dav

import (
	errors "errors"
	fmt "fmt"
	io "io"
	os "os"
	filepath "path/filepath"
	strconv "strconv"
	strings "strings"
	time "time"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
	models "zoterowebdav/server/models"
)

// leftoverAge is how old a hidden upload file has to be before dav-reconcile
// takes it for one a crash left behind. Nothing in progress is that old: an
// upload that stops moving for progressTimeout is cut off.
const leftoverAge = 24 * time.Hour

// accountQuota is an account's limit in bytes, 0 for none.
func ( server *Server ) accountQuota( user_id uint64 ) ( quota int64 , err error ) {
	account , err := models.GetDAVAccount( server.Store , user_id )
	if err != nil { return }
	quota = account.QuotaFor( server.cfg().WebDAV.DefaultQuotaBytes )
	return
}

// reindex reads an account's zotero/ folder into the attachment index.
func ( server *Server ) reindex( user_id uint64 ) ( err error ) {
	rows , err := server.scanZotero( user_id )
	if err != nil { return }
	err = models.ReplaceDAVAttachments( server.Store , user_id , rows )
	return
}

// scanZotero is the attachment index as the zotero/ folder has it.
func ( server *Server ) scanZotero( user_id uint64 ) ( rows []*models.DAVAttachment , err error ) {
	rows = []*models.DAVAttachment{}
	folder := filepath.Join( server.Root( user_id ) , "zotero" )
	entries , err := os.ReadDir( folder )
	if errors.Is( err , os.ErrNotExist ) {
		err = nil
		return
	}
	if err != nil { return }
	by_key := map[string]*models.DAVAttachment{}
	row := func( key string ) ( found *models.DAVAttachment ) {
		found = by_key[ key ]
		if found == nil {
			found = &models.DAVAttachment{ Key: key , UpdatedAt: time.Now().UTC() }
			by_key[ key ] = found
			rows = append( rows , found )
		}
		return
	}
	for _ , entry := range entries {
		match := zoteroFile.FindStringSubmatch( "/zotero/" + entry.Name() )
		if match == nil || entry.Type().IsRegular() == false { continue }
		full := filepath.Join( folder , entry.Name() )
		info , info_err := entry.Info()
		if info_err != nil { continue }
		switch match[ 2 ] {
		case "zip":
			found := row( match[ 1 ] )
			found.HasZip , found.ZipSize = true , plainInfo( full , info ).Size()
		case "prop":
			raw , read_err := server.readSmall( full )
			if read_err != nil { continue }
			mtime , md5 , parse_err := models.ParseZoteroProp( raw )
			if parse_err != nil { continue }
			found := row( match[ 1 ] )
			found.HasProp , found.MTime , found.MD5 = true , mtime , md5
		}
	}
	return
}

// readSmall reads a small file whole, decrypted.
func ( server *Server ) readSmall( full string ) ( raw []byte , err error ) {
	file , err := server.openRead( full , "" )
	if err != nil { return }
	defer file.Close()
	raw , err = io.ReadAll( io.LimitReader( file , headKeep ) )
	return
}

// Reconcile is the dav-reconcile job: for every account it counts the folder
// and its trash afresh, rebuilds the attachment index, and removes uploads a
// crash left behind and trash past webdav.trash_days. It removes the folders
// of accounts that no longer exist, which is how a deleted account's files go.
func ( server *Server ) Reconcile() ( note string , err error ) {
	cfg := server.cfg()
	accounts , err := models.ListDAVAccounts( server.Store )
	if err != nil { return }
	leftovers , trashed , gone := 0 , 0 , 0
	var problems []error
	for _ , account := range accounts {
		root := server.Root( account.UserID )
		leftovers += removeLeftovers( root )
		trashed += server.purgeTrash( account.UserID , time.Duration( cfg.WebDAV.TrashDays ) * 24 * time.Hour )
		used := treeSize( root ) + treeSize( server.TrashRoot( account.UserID ) )
		if set_err := server.setUsage( account.UserID , used ); set_err != nil && errors.Is( set_err , db.ErrNotFound ) == false {
			problems = append( problems , set_err )
		}
		if index_err := server.reindex( account.UserID ); index_err != nil {
			problems = append( problems , fmt.Errorf( "account %d: %w" , account.UserID , index_err ) )
		}
	}
	for _ , parent := range []string{ cfg.WebDAV.Dir , cfg.WebDAV.TrashDir } {
		removed , gone_err := server.removeGone( parent )
		gone += removed
		if gone_err != nil { problems = append( problems , gone_err ) }
	}
	note = fmt.Sprintf( "%d accounts counted, %d leftover uploads and %d trash entries removed, %d deleted accounts' folders removed" , len( accounts ) , leftovers , trashed , gone )
	err = errors.Join( problems... )
	return
}

// removeLeftovers deletes hidden upload files older than leftoverAge.
func removeLeftovers( root string ) ( removed int ) {
	cutoff := time.Now().Add( -leftoverAge )
	filepath.WalkDir( root , func( full string , entry os.DirEntry , err error ) error {
		if err != nil || entry.Type().IsRegular() == false || strings.HasPrefix( entry.Name() , reservedPrefix ) == false { return nil }
		if info , info_err := entry.Info(); info_err == nil && info.ModTime().Before( cutoff ) {
			if os.Remove( full ) == nil { removed += 1 }
		}
		return nil
	} )
	return
}

// removeGone removes the folders under parent named for an account id that
// has no user any more.
func ( server *Server ) removeGone( parent string ) ( removed int , err error ) {
	entries , err := os.ReadDir( parent )
	if err != nil { return }
	for _ , entry := range entries {
		user_id , parse_err := strconv.ParseUint( entry.Name() , 10 , 64 )
		if parse_err != nil || entry.IsDir() == false { continue }
		if _ , get_err := models.GetUser( server.Store , user_id ); errors.Is( get_err , db.ErrNotFound ) == false { continue }
		if remove_err := os.RemoveAll( filepath.Join( parent , entry.Name() ) ); remove_err != nil {
			err = errors.Join( err , remove_err )
			continue
		}
		server.mu.Lock()
		delete( server.users , user_id )
		server.mu.Unlock()
		removed += 1
	}
	return
}

// ConvertResult is what Convert did.
type ConvertResult struct {
	Converted int
	Skipped   int
}

// Convert rewrites every file under dir (or one account's) as encrypted or as
// plaintext, each atomically: `manage dav-convert`, with the server stopped.
// A file already in the wanted form is left alone.
func Convert( dir string , master_hex string , encrypt bool ) ( result ConvertResult , err error ) {
	err = filepath.WalkDir( dir , func( full string , entry os.DirEntry , walk_err error ) ( step_err error ) {
		if walk_err != nil {
			step_err = walk_err
			return
		}
		if entry.Type().IsRegular() == false || strings.HasPrefix( entry.Name() , reservedPrefix ) { return }
		info , step_err := entry.Info()
		if step_err != nil { return }
		source , step_err := os.Open( full )
		if step_err != nil { return }
		defer source.Close()
		head := make( []byte , 8 )
		read , _ := source.ReadAt( head , 0 )
		from := ""
		if encryption.IsSegmented( head[ :read ] ) {
			if _ , open_err := encryption.OpenSegments( source , info.Size() , master_hex ); open_err == nil { from = master_hex }
		}
		if ( from != "" ) == encrypt {
			result.Skipped += 1
			return
		}
		to := ""
		if encrypt { to = master_hex }
		temp , step_err := os.CreateTemp( filepath.Dir( full ) , reservedPrefix + "convert-*" )
		if step_err != nil { return }
		step_err = encryption.RewriteSegments( source , info.Size() , from , temp , to )
		if step_err == nil { step_err = temp.Sync() }
		if close_err := temp.Close(); step_err == nil { step_err = close_err }
		if step_err == nil { step_err = os.Chtimes( temp.Name() , info.ModTime() , info.ModTime() ) }
		if step_err == nil { step_err = os.Rename( temp.Name() , full ) }
		if step_err != nil {
			os.Remove( temp.Name() )
			step_err = fmt.Errorf( "%s: %w" , full , step_err )
			return
		}
		result.Converted += 1
		return
	} )
	return
}

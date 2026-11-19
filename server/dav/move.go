package dav

import (
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"

	models "zoterowebdav/server/models"
)

// ErrAttachmentTaken is a move onto a key whose files are already there.
var ErrAttachmentTaken = errors.New( "the new key already has files" )

// MoveAttachment renames zotero/<from>.zip and .prop to <to>, as an item
// moved to a new key (models.MoveLibraryItems) takes its files with it. The
// files keep their bytes (encryption at rest is not tied to the name), so
// this is two renames and the index row moving. moved is false when from has
// no files.
func ( server *Server ) MoveAttachment( user_id uint64 , from string , to string ) ( moved bool , err error ) {
	if models.ZoteroKeyPattern.MatchString( from ) == false || models.ZoteroKeyPattern.MatchString( to ) == false {
		err = errors.New( "keys are 8 of A-Z and 2-9" )
		return
	}
	folder := filepath.Join( server.Root( user_id ) , "zotero" )
	if err = noLinks( server.Root( user_id ) , "/zotero" ); err != nil { return }
	present := []string{}
	for _ , ext := range []string{ ".zip" , ".prop" } {
		if info , stat_err := os.Lstat( filepath.Join( folder , to + ext ) ); stat_err == nil && info.Mode().IsRegular() {
			err = ErrAttachmentTaken
			return
		}
		if info , stat_err := os.Lstat( filepath.Join( folder , from + ext ) ); stat_err == nil && info.Mode().IsRegular() {
			present = append( present , ext )
		}
	}
	if len( present ) == 0 { return }
	// The .zip first, as Zotero writes it: a .prop says the zip is whole.
	for _ , ext := range present {
		if err = os.Rename( filepath.Join( folder , from + ext ) , filepath.Join( folder , to + ext ) ); err != nil {
			err = fmt.Errorf( "moving %s: %w" , ext , err )
			server.reindex( user_id )
			return
		}
	}
	moved = true
	if index_err := models.MoveDAVAttachment( server.Store , user_id , from , to ); index_err != nil {
		// The folder is right; the index is rebuilt from it.
		err = server.reindex( user_id )
	}
	return
}

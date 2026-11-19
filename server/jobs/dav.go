package jobs

import (
	errors "errors"
	fmt "fmt"
	time "time"

	config "zoterowebdav/server/config"
	dav "zoterowebdav/server/dav"
	db "zoterowebdav/server/db"
	models "zoterowebdav/server/models"
)

// WebDAV registers the WebDAV jobs. dav-reconcile counts every folder afresh,
// rebuilds the attachment index, and clears out leftovers, old trash and the
// folders of deleted accounts (see dav.Server.Reconcile). It also drops dead
// app passwords and library tombstones past webdav.tombstone_days.
func WebDAV( registry *Registry , store *db.Store , settings *config.Holder , server *dav.Server ) {
	registry.Register( "dav-reconcile" , time.Hour , func() ( note string , err error ) {
		note , err = server.Reconcile()
		passwords , passwords_err := models.PurgeDeadDAVPasswords( store )
		tombstones , tombstones_err := models.PurgeLibraryTombstones( store , time.Duration( settings.Get().WebDAV.TombstoneDays ) * 24 * time.Hour )
		note += fmt.Sprintf( ", %d app passwords and %d library tombstones dropped" , passwords , tombstones )
		err = errors.Join( err , passwords_err , tombstones_err )
		return
	} )
}

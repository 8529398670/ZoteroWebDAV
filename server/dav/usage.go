package dav

import (
	log "log"
	os "os"
	filepath "path/filepath"
	strconv "strconv"

	models "zoterowebdav/server/models"
)

// Usage is the bytes on disk in an account's folder and its trash, encrypted
// files at their encrypted size. The running count lives here and in the
// account record; dav-reconcile counts it afresh every hour, which also mends
// whatever drift concurrent writes left.

// usedBytes is the account's running count, read from the database the
// first time.
func ( server *Server ) usedBytes( user_id uint64 , state *userState ) ( used int64 ) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.loaded == false {
		if account , err := models.GetDAVAccount( server.Store , user_id ); err == nil {
			state.used = account.UsedBytes
		}
		state.loaded = true
	}
	used = state.used
	return
}

// addUsage moves the count by delta and records it.
func ( server *Server ) addUsage( user_id uint64 , state *userState , delta int64 ) {
	if delta == 0 { return }
	server.usedBytes( user_id , state )
	state.mu.Lock()
	state.used = max( state.used + delta , 0 )
	used := state.used
	state.mu.Unlock()
	if err := models.SetDAVUsage( server.Store , user_id , used ); err != nil {
		log.Printf( "[webdav] could not record usage of account %d: %v" , user_id , err )
	}
}

// setUsage is dav-reconcile's count.
func ( server *Server ) setUsage( user_id uint64 , used int64 ) ( err error ) {
	state := server.state( user_id )
	state.mu.Lock()
	state.used , state.loaded = used , true
	state.mu.Unlock()
	err = models.SetDAVUsage( server.Store , user_id , used )
	return
}

// Usage is the account's running count, for the screens.
func ( server *Server ) Usage( user_id uint64 ) ( used int64 ) {
	used = server.usedBytes( user_id , server.state( user_id ) )
	return
}

func ( state *userState ) reserve( size int64 ) {
	state.mu.Lock()
	state.reserved += size
	state.mu.Unlock()
}

func ( state *userState ) release( size int64 ) {
	state.mu.Lock()
	state.reserved = max( state.reserved - size , 0 )
	state.mu.Unlock()
}

func ( state *userState ) reservedBytes() ( reserved int64 ) {
	state.mu.Lock()
	reserved = state.reserved
	state.mu.Unlock()
	return
}

// treeSize is the bytes on disk under full, a file or a folder, without
// following links.
func treeSize( full string ) ( size int64 ) {
	filepath.WalkDir( full , func( _ string , entry os.DirEntry , err error ) error {
		if err != nil { return nil }
		if entry.Type().IsRegular() {
			if info , info_err := entry.Info(); info_err == nil { size += info.Size() }
		}
		return nil
	} )
	return
}

// ensureRoot makes the account's folder, and zotero/ in it, the first time
// the account is seen: Zotero's Verify Server then finds what it needs
// without asking to create anything.
func ( server *Server ) ensureRoot( user_id uint64 , state *userState ) ( err error ) {
	state.mu.Lock()
	ready := state.rootReady
	state.mu.Unlock()
	if ready { return }
	if err = os.MkdirAll( filepath.Join( server.Root( user_id ) , "zotero" ) , 0o700 ); err != nil { return }
	state.mu.Lock()
	state.rootReady = true
	state.mu.Unlock()
	return
}

// EnsureRoot is ensureRoot for the screens, when an account first picks its
// name.
func ( server *Server ) EnsureRoot( user_id uint64 ) ( err error ) {
	err = server.ensureRoot( user_id , server.state( user_id ) )
	return
}

// ZoteroFolder is where an account's zotero/ folder is under dir.
func ZoteroFolder( dir string , user_id uint64 ) ( folder string ) {
	folder = filepath.Join( dir , strconv.FormatUint( user_id , 10 ) , "zotero" )
	return
}

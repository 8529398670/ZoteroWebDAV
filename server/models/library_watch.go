package models

import (
	sync "sync"
	time "time"
)

// Waiting for a library to change, so a client can hold one request open
// (GET /api/library/changes?wait=) instead of asking every few seconds.
//
// Two things change: the records (items and collections, each write a new
// rev) and the attachment index (a .zip or .prop landing or going over
// WebDAV, which has no rev). The second is counted here as a files stamp:
// a number that moves on every change to one library's index. It lives in
// memory and starts at the process's start time, so a restart moves it too,
// and a client that sees it move asks for the attachments changed since.

type libraryWatch struct {
	mutex   sync.Mutex
	waiters map[uint64][]chan struct{}
	files   map[uint64]int64
	boot    int64
}

var watch = &libraryWatch{ waiters: map[uint64][]chan struct{}{} , files: map[uint64]int64{} , boot: time.Now().UnixMicro() }

// libraryChanged wakes everyone waiting on library_id. files says the
// attachment index changed.
func libraryChanged( library_id uint64 , files bool ) {
	watch.mutex.Lock()
	defer watch.mutex.Unlock()
	if files {
		next := time.Now().UnixMicro()
		if previous := watch.stampLocked( library_id ); next <= previous { next = previous + 1 }
		watch.files[ library_id ] = next
	}
	for _ , waiter := range watch.waiters[ library_id ] { close( waiter ) }
	delete( watch.waiters , library_id )
}

func ( watch *libraryWatch ) stampLocked( library_id uint64 ) ( stamp int64 ) {
	stamp , found := watch.files[ library_id ]
	if found == false { stamp = watch.boot }
	return
}

// LibraryFilesStamp is library_id's files stamp now.
func LibraryFilesStamp( library_id uint64 ) ( stamp int64 ) {
	watch.mutex.Lock()
	defer watch.mutex.Unlock()
	stamp = watch.stampLocked( library_id )
	return
}

// WatchLibrary is a channel closed at library_id's next change. Take it
// before looking at the library, so a change between the look and the wait
// is not missed.
func WatchLibrary( library_id uint64 ) ( changed <-chan struct{} ) {
	waiter := make( chan struct{} )
	watch.mutex.Lock()
	watch.waiters[ library_id ] = append( watch.waiters[ library_id ] , waiter )
	watch.mutex.Unlock()
	changed = waiter
	return
}

// UnwatchLibrary drops a channel WatchLibrary gave that timed out unused.
func UnwatchLibrary( library_id uint64 , changed <-chan struct{} ) {
	watch.mutex.Lock()
	defer watch.mutex.Unlock()
	kept := watch.waiters[ library_id ][ :0 ]
	for _ , waiter := range watch.waiters[ library_id ] {
		if ( <-chan struct{} )( waiter ) != changed { kept = append( kept , waiter ) }
	}
	if len( kept ) == 0 {
		delete( watch.waiters , library_id )
		return
	}
	watch.waiters[ library_id ] = kept
}

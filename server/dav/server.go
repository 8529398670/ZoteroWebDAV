// Package dav is the WebDAV server: Zotero's file sync, and any other WebDAV
// client, each person in their own folder.
//
// It is a plain net/http server on its own port (webdav.port), not a Fiber
// route. Fiber hands a net/http handler the whole request body as one byte
// slice, capped at server.body_limit_bytes, and a Zotero attachment is one
// zip of up to webdav.max_file_mb -- it has to stream to disk. WebDAV also
// signs in with HTTP Basic auth and an app password, where the app signs in
// with cookies and Bearer keys, so the two share nothing at the door anyway.
// They share the database, the settings and the activity log, through Server.
//
// The protocol itself is golang.org/x/net/webdav. What this package adds is
// around it:
//
//	server.go   the listener and its timeouts, the per-account state
//	handler.go  each request: headers, method list, maintenance, sign-in,
//	            the account's own folder, rate limit, request checks, logging
//	auth.go     Basic auth with an app password, and refusing guessers
//	fs.go       webdav.FileSystem on <app dir>/dav/<user id>/: no symlinks,
//	            atomic writes, quota, the attachment index
//	file.go     the files it hands out, plain or encrypted
//	usage.go    how much each folder holds, and reserving room for uploads
//	browse.go   the same folders for the WebDAV screen (list, download, trash)
//	zip.go      one file out of zotero/<KEY>.zip, for the library API
//	reconcile.go the hourly job: usage, index, leftovers, trash, gone accounts
package dav

import (
	context "context"
	errors "errors"
	log "log"
	http "net/http"
	strconv "strconv"
	filepath "path/filepath"
	sync "sync"
	time "time"

	webdav "golang.org/x/net/webdav"

	config "zoterowebdav/server/config"
	db "zoterowebdav/server/db"
	logs "zoterowebdav/server/logs"
	models "zoterowebdav/server/models"
)

const (
	// readHeaderTimeout bounds a client sending its headers slowly.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout is how long a kept-alive connection waits for its next
	// request. Zotero reuses connections across a sync.
	idleTimeout = 120 * time.Second
	// progressTimeout is how long a body may go without a byte moving, in
	// either direction. There is no limit on a whole request: a 1 GB upload
	// on a slow line takes as long as it takes, so long as it keeps moving.
	progressTimeout = 60 * time.Second
	maxHeaderBytes  = 32 * 1024
	// xmlBodyLimit caps the body of everything but PUT: PROPFIND, PROPPATCH
	// and LOCK bodies are a few hundred bytes of XML.
	xmlBodyLimit = 1024 * 1024
	shutdownWait = 10 * time.Second
)

// Server is the WebDAV side of the app.
type Server struct {
	Store    *db.Store
	Settings *config.Holder
	Logger   *logs.Logger

	// failures counts refused sign-ins per address: five, then fifteen
	// minutes of 429s. Only per address -- an app password is 256 random
	// bits, so guessing gets nowhere, and locking a name would let anyone
	// stop that person's Zotero from syncing.
	failures *models.AttemptGuard[ string ]

	mu    sync.Mutex
	users map[ uint64 ]*userState

	listener *http.Server
}

// userState is what the server keeps per account while it runs.
type userState struct {
	mu        sync.Mutex
	used      int64
	loaded    bool
	reserved  int64
	locks     webdav.LockSystem
	tokens    float64
	refilled  time.Time
	rootReady bool
}

func New( store *db.Store , settings *config.Holder , logger *logs.Logger ) ( server *Server ) {
	server = &Server{
		Store:    store,
		Settings: settings,
		Logger:   logger,
		failures: models.NewAttemptGuard[ string ]( models.AttemptMaxFailures ),
		users:    map[ uint64 ]*userState{},
	}
	return
}

func ( server *Server ) cfg() ( cfg *config.Config ) {
	cfg = server.Settings.Get()
	return
}

func ( server *Server ) state( user_id uint64 ) ( state *userState ) {
	server.mu.Lock()
	defer server.mu.Unlock()
	state = server.users[ user_id ]
	if state == nil {
		state = &userState{ locks: webdav.NewMemLS() }
		server.users[ user_id ] = state
	}
	return
}

// Root is the folder an account's WebDAV files are in.
func ( server *Server ) Root( user_id uint64 ) ( root string ) {
	root = filepath.Join( server.cfg().WebDAV.Dir , strconv.FormatUint( user_id , 10 ) )
	return
}

// TrashRoot is where the WebDAV screen's deletes of one account go.
func ( server *Server ) TrashRoot( user_id uint64 ) ( root string ) {
	root = filepath.Join( server.cfg().WebDAV.TrashDir , strconv.FormatUint( user_id , 10 ) )
	return
}

// MasterKey is the key encrypted files' keys are sealed with.
func ( server *Server ) MasterKey() ( key_hex string , err error ) {
	key_hex , err = models.DAVMasterKey( server.Store , server.cfg().SecretKey )
	return
}

// Listen starts the listener in the background. It returns once the port is
// bound, so a port in use stops the server starting rather than failing
// later in a goroutine nobody watches.
func ( server *Server ) Listen() ( err error ) {
	cfg := server.cfg()
	server.listener = &http.Server{
		Addr:              cfg.WebDAV.ListenAddress(),
		Handler:           server,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New( log.Writer() , "[webdav] " , log.LstdFlags ),
	}
	listening , err := listen( server.listener.Addr )
	if err != nil { return }
	go func() {
		if serve_err := server.listener.Serve( listening ); serve_err != nil && errors.Is( serve_err , http.ErrServerClosed ) == false {
			log.Printf( "[webdav] stopped: %v" , serve_err )
		}
	}()
	return
}

// Close stops accepting requests and waits a while for the ones running.
func ( server *Server ) Close() {
	if server.listener == nil { return }
	ctx , cancel := context.WithTimeout( context.Background() , shutdownWait )
	defer cancel()
	server.listener.Shutdown( ctx )
}

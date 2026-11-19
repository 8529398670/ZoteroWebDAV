package dav

import (
	context "context"
	fmt "fmt"
	io "io"
	log "log"
	http "net/http"
	url "net/url"
	path "path"
	strconv "strconv"
	strings "strings"
	time "time"

	webdav "golang.org/x/net/webdav"

	config "zoterowebdav/server/config"
	disk "zoterowebdav/server/disk"
	encryption "zoterowebdav/server/encryption"
	logs "zoterowebdav/server/logs"
)

// The methods a WebDAV client needs. Anything else is 405.
var allowedMethods = map[string]bool{
	"OPTIONS": true , "PROPFIND": true , "PROPPATCH": true , "MKCOL": true ,
	"GET": true , "HEAD": true , "PUT": true , "DELETE": true ,
	"COPY": true , "MOVE": true , "LOCK": true , "UNLOCK": true ,
}

// writeMethods change something: refused in read_only maintenance, and
// always logged.
var writeMethods = map[string]bool{
	"PROPPATCH": true , "MKCOL": true , "PUT": true , "DELETE": true ,
	"COPY": true , "MOVE": true , "LOCK": true , "UNLOCK": true ,
}

const allowHeader = "OPTIONS, PROPFIND, PROPPATCH, MKCOL, GET, HEAD, PUT, DELETE, COPY, MOVE, LOCK, UNLOCK"

type contextKey int

const putStateKey contextKey = 1

// putState is how a PUT's file learns whether the whole body arrived. A
// client that goes away half way leaves a short body; x/net/webdav closes the
// file all the same, and the file must not take the place of the old one.
type putState struct {
	complete bool
}

// ServeHTTP is one WebDAV request, start to finish.
func ( server *Server ) ServeHTTP( w http.ResponseWriter , r *http.Request ) {
	started := time.Now()
	controller := http.NewResponseController( w )
	recorder := &recorder{ ResponseWriter: w , controller: controller , status: http.StatusOK }
	w = recorder
	cfg := server.cfg()
	address := clientAddress( r , cfg )
	var who *signedIn

	defer func() {
		if failure := recover(); failure != nil {
			log.Printf( "[webdav] panic: %s %s: %v" , r.Method , r.URL.Path , failure )
			if recorder.wrote == false { http.Error( w , "internal error" , http.StatusInternalServerError ) }
			recorder.status = http.StatusInternalServerError
		}
		server.logRequest( r , recorder , who , address , started , cfg )
	}()

	headers := w.Header()
	headers.Set( "X-Content-Type-Options" , "nosniff" )
	headers.Set( "Content-Security-Policy" , "sandbox; default-src 'none'" )
	headers.Set( "X-Robots-Tag" , "noindex, nofollow" )
	headers.Set( "Referrer-Policy" , "no-referrer" )
	headers.Set( "Cache-Control" , "no-store" )

	if allowedMethods[ r.Method ] == false {
		headers.Set( "Allow" , allowHeader )
		http.Error( w , "method not allowed" , http.StatusMethodNotAllowed )
		return
	}

	switch cfg.MaintenanceMode() {
	case config.MaintenanceFull:
		unavailable( w )
		return
	case config.MaintenanceReadOnly:
		if writeMethods[ r.Method ] {
			unavailable( w )
			return
		}
	}

	// Everything is behind sign-in, even a path that names nobody, so the
	// answer never says whether a name exists.
	who , result := server.authenticate( r , address )
	switch result {
	case authMissing:
		challenge( w )
		return
	case authFailed:
		server.logFailure( r , address , "webdav sign-in refused" )
		challenge( w )
		return
	case authLocked:
		headers.Set( "Retry-After" , strconv.Itoa( int( 15 * time.Minute / time.Second ) ) )
		http.Error( w , "too many failed sign-ins from this address -- try again later" , http.StatusTooManyRequests )
		return
	case authOff:
		http.Error( w , "WebDAV is switched off for this account" , http.StatusForbidden )
		return
	}

	// The folder in the path has to be the signed-in account's own.
	base := cfg.WebDAV.Prefix + "/" + who.account.Name
	if r.URL.Path != base && strings.HasPrefix( r.URL.Path , base + "/" ) == false {
		http.Error( w , "not found" , http.StatusNotFound )
		return
	}

	state := server.state( who.account.ID )
	if server.allow( state , cfg ) == false {
		headers.Set( "Retry-After" , "1" )
		http.Error( w , "slow down" , http.StatusTooManyRequests )
		return
	}
	if err := server.ensureRoot( who.account.ID , state ); err != nil {
		log.Printf( "[webdav] could not create the folder of account %d: %v" , who.account.ID , err )
		http.Error( w , "internal error" , http.StatusInternalServerError )
		return
	}

	// Zotero always says Depth 0 or 1. Infinity walks a whole tree in one
	// request, so it is refused the way RFC 4918 allows.
	if r.Method == "PROPFIND" {
		if depth := r.Header.Get( "Depth" ); depth != "0" && depth != "1" {
			headers.Set( "Content-Type" , "application/xml; charset=utf-8" )
			w.WriteHeader( http.StatusForbidden )
			io.WriteString( w , `<?xml version="1.0" encoding="utf-8"?><D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>` )
			return
		}
	}

	// COPY and MOVE may only land in the same folder. x/net/webdav checks
	// the prefix as a string, so /alice would accept /alice2/...; this is
	// the exact check.
	if r.Method == "COPY" || r.Method == "MOVE" {
		destination , err := url.Parse( r.Header.Get( "Destination" ) )
		if err != nil {
			http.Error( w , "bad Destination" , http.StatusBadRequest )
			return
		}
		cleaned := path.Clean( destination.Path )
		if cleaned != base && strings.HasPrefix( cleaned , base + "/" ) == false {
			http.Error( w , "Destination must be in your own folder" , http.StatusForbidden )
			return
		}
	}

	ctx := r.Context()
	if r.Method == "PUT" {
		if status , message := server.admitUpload( r , who , state , cfg ); status != 0 {
			http.Error( w , message , status )
			return
		}
		reserved := max( r.ContentLength , 0 )
		state.reserve( reserved )
		defer state.release( reserved )
		put := &putState{}
		ctx = context.WithValue( ctx , putStateKey , put )
		r.Body = &progressReader{ body: r.Body , controller: controller , put: put }
	} else {
		r.Body = http.MaxBytesReader( w , r.Body , xmlBodyLimit )
	}

	handler := &webdav.Handler{
		Prefix:     base,
		FileSystem: &userFS{ server: server , userID: who.account.ID , root: server.Root( who.account.ID ) , state: state },
		LockSystem: state.locks,
	}
	handler.ServeHTTP( w , r.WithContext( ctx ) )
}

// admitUpload is everything that can refuse a PUT before a byte is read: too
// big, no room left in the quota, no room left on the disk. A body with no
// Content-Length is held to the same limits as it streams (fs.go).
func ( server *Server ) admitUpload( r *http.Request , who *signedIn , state *userState , cfg *config.Config ) ( status int , message string ) {
	length := r.ContentLength
	if length > cfg.WebDAV.MaxFileBytes {
		status , message = http.StatusRequestEntityTooLarge , fmt.Sprintf( "files are limited to %d MB here" , cfg.WebDAV.MaxFileBytes / 1024 / 1024 )
		return
	}
	stored := max( length , 0 )
	if cfg.WebDAV.EncryptAtRest {
		stored += encryption.SegmentHeaderSize + ( stored / encryption.SegmentSize + 1 ) * 16
	}
	quota := who.account.QuotaFor( cfg.WebDAV.DefaultQuotaBytes )
	if quota > 0 {
		used := server.usedBytes( who.account.ID , state )
		if used + state.reservedBytes() + stored > quota {
			status , message = http.StatusInsufficientStorage , "your WebDAV folder is full"
			return
		}
	}
	if cfg.WebDAV.MinFreeBytes > 0 {
		if free , _ , err := disk.Space( cfg.WebDAV.Dir ); err == nil && int64( free ) - stored < cfg.WebDAV.MinFreeBytes {
			status , message = http.StatusInsufficientStorage , "the server is out of space"
			return
		}
	}
	return
}

// allow is the per-account rate limit: a token bucket of rate_burst tokens
// refilled at rate_per_second.
func ( server *Server ) allow( state *userState , cfg *config.Config ) ( ok bool ) {
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	burst := float64( cfg.WebDAV.RateBurst )
	if state.refilled.IsZero() {
		state.tokens = burst
	} else {
		state.tokens = min( burst , state.tokens + now.Sub( state.refilled ).Seconds() * float64( cfg.WebDAV.RatePerSecond ) )
	}
	state.refilled = now
	if state.tokens >= 1 {
		state.tokens -= 1
		ok = true
	}
	return
}

func challenge( w http.ResponseWriter ) {
	w.Header().Set( "WWW-Authenticate" , `Basic realm="ZoteroWebDAV", charset="UTF-8"` )
	http.Error( w , "sign in with your WebDAV name and an app password" , http.StatusUnauthorized )
}

func unavailable( w http.ResponseWriter ) {
	w.Header().Set( "Retry-After" , "300" )
	http.Error( w , "down for maintenance" , http.StatusServiceUnavailable )
}

// progressReader is a PUT body: each read must bring something within
// progressTimeout, and reaching the end is what marks the upload whole.
type progressReader struct {
	body       io.ReadCloser
	controller *http.ResponseController
	put        *putState
}

func ( reader *progressReader ) Read( data []byte ) ( read int , err error ) {
	reader.controller.SetReadDeadline( time.Now().Add( progressTimeout ) )
	read , err = reader.body.Read( data )
	if err == io.EOF { reader.put.complete = true }
	return
}

func ( reader *progressReader ) Close() ( err error ) {
	err = reader.body.Close()
	return
}

// recorder notes what was answered, for the log, and holds every write to
// progressTimeout.
type recorder struct {
	http.ResponseWriter
	controller *http.ResponseController
	status     int
	bytes      int
	wrote      bool
}

func ( writer *recorder ) WriteHeader( status int ) {
	if writer.wrote == false {
		writer.status = status
		writer.wrote = true
	}
	writer.ResponseWriter.WriteHeader( status )
}

func ( writer *recorder ) Write( data []byte ) ( written int , err error ) {
	writer.wrote = true
	writer.controller.SetWriteDeadline( time.Now().Add( progressTimeout ) )
	written , err = writer.ResponseWriter.Write( data )
	writer.bytes += written
	return
}

func ( writer *recorder ) Unwrap() ( inner http.ResponseWriter ) {
	inner = writer.ResponseWriter
	return
}

// logRequest writes the request to the activity log: every write, every
// refusal, and reads too with webdav.log_reads.
func ( server *Server ) logRequest( r *http.Request , recorder *recorder , who *signedIn , address string , started time.Time , cfg *config.Config ) {
	if server.Logger == nil || server.Logger.Enabled() == false { return }
	worth := writeMethods[ r.Method ] || recorder.status >= 500 || cfg.WebDAV.LogReads
	// A 401 with no credentials is every client's first request.
	if recorder.status == http.StatusUnauthorized { worth = false }
	if worth == false { return }
	level := logs.LevelInfo
	if recorder.status >= 500 { level = logs.LevelError }
	entry := logs.Entry{
		Kind:  logs.KindRequest,
		Level: level,
		IP:    address,
		UA:    logs.Truncate( r.UserAgent() , 300 ),
		Request: &logs.Request{
			Method:   r.Method,
			Path:     logs.Truncate( r.URL.Path , 500 ),
			Status:   recorder.status,
			MS:       float64( time.Since( started ).Microseconds() ) / 1000,
			BytesOut: recorder.bytes,
		},
		Msg: "webdav",
	}
	if who != nil {
		entry.Actor = &logs.Actor{ UserID: who.user.ID , Name: who.user.DisplayName , Role: who.user.Role , Method: "webdav" , KeyID: who.passwordID }
	}
	server.Logger.Log( entry )
}

func ( server *Server ) logFailure( r *http.Request , address string , message string ) {
	if server.Logger == nil || server.Logger.Enabled() == false { return }
	name , _ , _ := r.BasicAuth()
	server.Logger.Log( logs.Entry{
		Kind:   logs.KindAuth,
		Level:  logs.LevelWarn,
		IP:     address,
		UA:     logs.Truncate( r.UserAgent() , 300 ),
		Msg:    message,
		Detail: map[string]any{ "name": logs.Truncate( name , 64 ) , "method": r.Method },
	} )
}

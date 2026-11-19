package routes

import (
	bufio "bufio"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	io "io"
	log "log"
	net "net"
	os "os"
	filepath "path/filepath"
	strings "strings"
	sync "sync"
	time "time"

	fiber "github.com/gofiber/fiber/v3"

	apidoc "zoterowebdav/server/apidoc"
	dav "zoterowebdav/server/dav"
	models "zoterowebdav/server/models"
	security "zoterowebdav/server/security"
	zotio "zoterowebdav/server/zotio"
)

// The Import / Export screen's API. An import reads a Zotero export (RDF,
// RIS or CSV, alone or zipped with its files) or, for the owner, a Zotero
// data folder on this machine, into the library, and puts attachment files
// into the WebDAV folder as Zotero's own sync would. An export writes the
// library back out as something Zotero's File > Import reads.
//
// An upload comes in pieces of ImportChunkBytes: the API's body limit and
// read timeout are made for small JSON requests, and an export with its
// files can be gigabytes. One import runs at a time per account, in the
// background; the page polls its status.

// ImportChunkBytes is the size of one upload piece. main.go lets bodies this
// big through, and middleware.BodyLimit holds every other route to
// server.body_limit_bytes.
const ImportChunkBytes = 4 * 1024 * 1024

// ImportChunkPath is the one route bodies of ImportChunkBytes may reach.
const ImportChunkPath = "/api/library/import/chunk"

// importBatch is how many records go to the database in one transaction.
const importBatch = 100

type importStatus struct {
	Running      bool      `json:"running"`
	Done         bool      `json:"done"`
	Phase        string    `json:"phase,omitempty" jsonschema:"enum=reading,enum=collections,enum=items,enum=files,enum=cleanup,enum=done,enum=failed"`
	Source       string    `json:"source,omitempty" jsonschema_description:"What was imported: the upload's name, or the data folder."`
	Format       string    `json:"format,omitempty"`
	Mode         string    `json:"mode,omitempty" jsonschema:"enum=merge,enum=replace"`
	Collections  int       `json:"collections"`
	Items        int       `json:"items"`
	Changed      int       `json:"changed" jsonschema_description:"Records that were new or different."`
	Processed        int       `json:"processed" jsonschema_description:"Of collections + items + files, how many are done."`
	Total        int       `json:"total" jsonschema_description:"collections + items + files to do."`
	Files        int       `json:"files" jsonschema_description:"Attachments whose files were stored."`
	FilesSkipped int       `json:"files_skipped" jsonschema_description:"Attachments already in the WebDAV folder with the same file."`
	FilesFailed  int       `json:"files_failed"`
	Deleted      int       `json:"deleted" jsonschema_description:"Records removed by a replace import."`
	Warnings     []string  `json:"warnings"`
	Error        string    `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
}

func ( importStatus ) APIName() ( name string ) { name = "ImportStatus"; return }

// importState is one account's upload and import.
type importState struct {
	mu     sync.Mutex
	status importStatus
	upload *pendingUpload
}

type pendingUpload struct {
	path     string
	name     string
	format   string
	mode     string
	size     int64
	received int64
}

var imports = struct {
	mu       sync.Mutex
	accounts map[uint64]*importState
}{ accounts: map[uint64]*importState{} }

func importFor( user_id uint64 ) ( state *importState ) {
	imports.mu.Lock()
	defer imports.mu.Unlock()
	state = imports.accounts[ user_id ]
	if state == nil {
		state = &importState{ status: importStatus{ Warnings: []string{} } }
		imports.accounts[ user_id ] = state
	}
	return
}

func ( state *importState ) snapshot() ( status importStatus ) {
	state.mu.Lock()
	defer state.mu.Unlock()
	status = state.status
	status.Warnings = append( []string{} , state.status.Warnings... )
	return
}

func ( state *importState ) update( change func( status *importStatus ) ) {
	state.mu.Lock()
	defer state.mu.Unlock()
	change( &state.status )
}

// importDir is where uploads wait while they arrive.
func ( handlers *Handlers ) importDir() ( dir string , err error ) {
	dir = filepath.Join( handlers.cfg().AppDir , "imports" )
	err = os.MkdirAll( dir , 0o700 )
	return
}

// ---------------------------------------------------------------------------
// formats
// ---------------------------------------------------------------------------

type formatsReply struct {
	Formats    []zotio.Format `json:"formats"`
	LocalPaths bool           `json:"local_paths" jsonschema_description:"Whether you may import a Zotero data folder on the server's machine: the owner, with webdav.import_local_paths on."`
	ChunkBytes int            `json:"chunk_bytes" jsonschema_description:"Send an upload in pieces of this size."`
	MaxBytes   int64          `json:"max_bytes" jsonschema_description:"The largest upload accepted."`
}

// libraryFormats is what the Import / Export screen offers.
//
// @route("GET /api/library/formats", summary="Import and export formats",
//     tag="Library", feature="webdav",
//     description="The formats the screen lists, with what each can do; those with neither import nor "
//         "export are not done yet.",
// )
func ( handlers *Handlers ) libraryFormats( c fiber.Ctx , in *apidoc.None ) ( reply *formatsReply , err error ) {
	cfg := handlers.cfg()
	reply = &formatsReply{ Formats: zotio.Formats , ChunkBytes: ImportChunkBytes , MaxBytes: cfg.WebDAV.ImportMaxBytes }
	if cfg.WebDAV.ImportLocalPaths {
		actor := security.ActorFrom( c )
		owner_id , owner_err := models.OwnerID( handlers.Store )
		reply.LocalPaths = owner_err == nil && actor != nil && actor.ViaAPIKey() == false && actor.User.ID == owner_id
	}
	return
}

// ---------------------------------------------------------------------------
// uploading
// ---------------------------------------------------------------------------

type importBeginRequest struct {
	libraryQuery
	Format string `json:"format" jsonschema:"enum=rdf,enum=ris,enum=csv" error:"format must be rdf, ris or csv"`
	Mode   string `json:"mode,omitempty" jsonschema:"enum=merge,enum=replace" jsonschema_description:"merge (the default) adds and updates; replace also removes what the import does not have." error:"mode must be merge or replace"`
	Name   string `json:"name" jsonschema:"minLength=1,maxLength=255" jsonschema_description:"The file's name, for the status." error:"name must be 1-255 characters"`
	Size   int64  `json:"size" jsonschema:"minimum=1" jsonschema_description:"The file's size in bytes." error:"size must be 1 or more"`
}

type importBeginReply struct {
	ChunkBytes int `json:"chunk_bytes"`
}

func ( state *importState ) busy() ( running bool ) {
	state.mu.Lock()
	running = state.status.Running
	state.mu.Unlock()
	return
}

// importBegin starts an upload, throwing away any unfinished one.
//
// @route("POST /api/library/import/begin", summary="Start uploading an export",
//     tag="Library", feature="webdav", errors=(409,),
//     description="Then send the file in pieces to POST /api/library/import/chunk, and start the import with "
//         "POST /api/library/import/finish. The file is a Zotero export, or a zip of the folder Zotero makes "
//         "with Export Files ticked.",
// )
func ( handlers *Handlers ) importBegin( c fiber.Ctx , in *importBeginRequest ) ( reply *importBeginReply , err error ) {
	format , found := zotio.FindFormat( in.Format )
	if found == false || format.Import == false {
		err = refuse( fiber.StatusBadRequest , zotio.ErrFormat.Error() )
		return
	}
	if in.Mode != "" && in.Mode != "merge" && in.Mode != "replace" {
		err = refuse( fiber.StatusBadRequest , "mode must be merge or replace" )
		return
	}
	if limit := handlers.cfg().WebDAV.ImportMaxBytes; in.Size > limit {
		err = refuse( fiber.StatusBadRequest , fmt.Sprintf( "uploads are limited to %d MB here" , limit / 1024 / 1024 ) )
		return
	}
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	state := importFor( user_id )
	if state.busy() {
		err = refuse( fiber.StatusConflict , "an import is already running" )
		return
	}
	dir , err := handlers.importDir()
	if err != nil { return }
	file , err := os.CreateTemp( dir , fmt.Sprintf( "upload-%d-*" , user_id ) )
	if err != nil { return }
	file.Close()
	mode := in.Mode
	if mode == "" { mode = "merge" }
	state.mu.Lock()
	if state.upload != nil { os.Remove( state.upload.path ) }
	state.upload = &pendingUpload{ path: file.Name() , name: in.Name , format: in.Format , mode: mode , size: in.Size }
	state.mu.Unlock()
	reply = &importBeginReply{ ChunkBytes: ImportChunkBytes }
	return
}

type importChunkRequest struct {
	libraryQuery
	Offset int64 `json:"offset" query:"offset" jsonschema:"minimum=0" jsonschema_description:"Where this piece starts: the bytes sent so far." error:"offset must be 0 or more"`
}

type importChunkReply struct {
	Received int64 `json:"received"`
}

// importChunk appends one piece of the upload.
//
// @route("POST /api/library/import/chunk", summary="Send a piece of the export",
//     tag="Library", feature="webdav", errors=(409,), body_type="application/octet-stream",
//     description="The body is the next piece, at most chunk_bytes, starting at offset. A piece that does not "
//         "start where the last ended is refused with the offset expected.",
// )
func ( handlers *Handlers ) importChunk( c fiber.Ctx , in *importChunkRequest ) ( reply *importChunkReply , err error ) {
	body := c.Body()
	if len( body ) == 0 || len( body ) > ImportChunkBytes {
		err = refuse( fiber.StatusBadRequest , fmt.Sprintf( "a piece is 1 to %d bytes" , ImportChunkBytes ) )
		return
	}
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	state := importFor( library_id )
	state.mu.Lock()
	defer state.mu.Unlock()
	upload := state.upload
	switch {
	case upload == nil:
		err = refuse( fiber.StatusConflict , "no upload started" )
		return
	case in.Offset != upload.received:
		err = refuse( fiber.StatusConflict , "that piece is out of order" , fiber.Map{ "received": upload.received } )
		return
	case upload.received + int64( len( body ) ) > upload.size:
		err = refuse( fiber.StatusBadRequest , "more than the size given" )
		return
	}
	file , err := os.OpenFile( upload.path , os.O_WRONLY | os.O_APPEND , 0o600 )
	if err != nil { return }
	_ , err = file.Write( body )
	if close_err := file.Close(); err == nil { err = close_err }
	if err != nil { return }
	upload.received += int64( len( body ) )
	reply = &importChunkReply{ Received: upload.received }
	return
}

// importFinish starts importing the upload.
//
// @route("POST /api/library/import/finish", summary="Import the uploaded export",
//     tag="Library", feature="webdav", errors=(409,), status=202,
//     description="Starts the import in the background. GET /api/library/import/status says how it goes.",
// )
func ( handlers *Handlers ) importFinish( c fiber.Ctx , in *libraryQuery ) ( reply *importStatus , err error ) {
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	state := importFor( user_id )
	state.mu.Lock()
	upload := state.upload
	switch {
	case state.status.Running:
		state.mu.Unlock()
		err = refuse( fiber.StatusConflict , "an import is already running" )
		return
	case upload == nil || upload.received != upload.size:
		state.mu.Unlock()
		err = refuse( fiber.StatusConflict , "the upload is not complete" )
		return
	}
	state.upload = nil
	state.status = importStatus{ Running: true , Phase: "reading" , Source: upload.name , Format: upload.format , Mode: upload.mode , Warnings: []string{} , StartedAt: time.Now().UTC() }
	state.mu.Unlock()
	format , _ := zotio.FindFormat( upload.format )
	// The upload stays open until the import ends: its files are read from
	// it as they are copied.
	go handlers.runImport( user_id , state , upload.mode , func() ( library *zotio.Library , done func() , read_err error ) {
		done = func() { os.Remove( upload.path ) }
		opened , read_err := zotio.OpenUpload( upload.path , format )
		if read_err != nil { return }
		done = func() {
			opened.Close()
			os.Remove( upload.path )
		}
		library , read_err = zotio.Read( format.ID , opened )
		return
	} )
	status := state.snapshot()
	reply = &status
	return
}

type importLocalRequest struct {
	libraryQuery
	Path  string `json:"path" jsonschema:"minLength=1,maxLength=4096" jsonschema_description:"The Zotero data folder (the one with zotero.sqlite), on the server's machine. ~ is your home folder." error:"path is required"`
	Mode  string `json:"mode,omitempty" jsonschema:"enum=merge,enum=replace" error:"mode must be merge or replace"`
	Files bool   `json:"files,omitempty" jsonschema_description:"Also copy each stored attachment's files from storage/ into your WebDAV folder."`
}

// importLocal imports a Zotero data folder on this machine.
//
// @route("POST /api/library/import/local", summary="Import a Zotero data folder on the server",
//     tag="Library", role=OWNER, feature="webdav", errors=(409,), status=202,
//     description="Reads zotero.sqlite (a copy, so a running Zotero is fine) and, with files, storage/. Only "
//         "with webdav.import_local_paths on. Items keep their Zotero keys.",
// )
func ( handlers *Handlers ) importLocal( c fiber.Ctx , in *importLocalRequest ) ( reply *importStatus , err error ) {
	if handlers.cfg().WebDAV.ImportLocalPaths == false {
		err = refuse( fiber.StatusNotFound , "importing a folder on the server is switched off (webdav.import_local_paths)" )
		return
	}
	if in.Mode != "" && in.Mode != "merge" && in.Mode != "replace" {
		err = refuse( fiber.StatusBadRequest , "mode must be merge or replace" )
		return
	}
	folder , find_err := zotio.FindDataFolder( in.Path )
	if find_err != nil {
		err = refuse( fiber.StatusBadRequest , find_err.Error() )
		return
	}
	dir , err := handlers.importDir()
	if err != nil { return }
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	state := importFor( user_id )
	mode := in.Mode
	if mode == "" { mode = "merge" }
	state.mu.Lock()
	if state.status.Running {
		state.mu.Unlock()
		err = refuse( fiber.StatusConflict , "an import is already running" )
		return
	}
	state.status = importStatus{ Running: true , Phase: "reading" , Source: folder , Format: "zotero" , Mode: mode , Warnings: []string{} , StartedAt: time.Now().UTC() }
	state.mu.Unlock()
	with_files := in.Files
	go handlers.runImport( user_id , state , mode , func() ( library *zotio.Library , done func() , read_err error ) {
		done = func() {}
		library , read_err = zotio.ReadDataFolder( folder , dir , with_files )
		return
	} )
	status := state.snapshot()
	reply = &status
	return
}

// importStatusRoute is the current or last import.
//
// @route("GET /api/library/import/status", summary="How the import is going",
//     tag="Library", feature="webdav",
// )
func ( handlers *Handlers ) importStatusRoute( c fiber.Ctx , in *libraryQuery ) ( reply *importStatus , err error ) {
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	status := importFor( library_id ).snapshot()
	reply = &status
	return
}

// ---------------------------------------------------------------------------
// the import itself
// ---------------------------------------------------------------------------

func ( handlers *Handlers ) runImport( user_id uint64 , state *importState , mode string , read func() ( *zotio.Library , func() , error ) ) {
	fail := func( err error ) {
		state.update( func( status *importStatus ) {
			status.Running , status.Phase , status.Error , status.FinishedAt = false , "failed" , err.Error() , time.Now().UTC()
		} )
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf( "[import] account %d: %v" , user_id , recovered )
			fail( fmt.Errorf( "the import stopped: %v" , recovered ) )
		}
	}()
	library , done , err := read()
	if done != nil { defer done() }
	if err != nil {
		fail( err )
		return
	}
	// Records with no key of their own find the ones already here.
	stored := map[string][]zotio.Item{}
	for _ , kind := range []string{ models.LibraryKindItem , models.LibraryKindCollection } {
		records , list_err := models.ListLibrary( handlers.Store , user_id , kind )
		if list_err != nil {
			fail( list_err )
			return
		}
		for _ , record := range records {
			data := zotio.Item{}
			if json.Unmarshal( record.Data , &data ) == nil {
				data[ "key" ] = record.Key
				stored[ kind ] = append( stored[ kind ] , data )
			}
		}
	}
	zotio.Match( library , stored[ models.LibraryKindItem ] , stored[ models.LibraryKindCollection ] )
	attachments := 0
	for _ , item := range library.Items {
		if len( library.Files[ zotio.Str( item , "key" ) ] ) > 0 { attachments += 1 }
	}
	state.update( func( status *importStatus ) {
		status.Collections , status.Items = len( library.Collections ) , len( library.Items )
		status.Total = len( library.Collections ) + len( library.Items ) + attachments
		status.Warnings = append( status.Warnings , library.Warnings... )
	} )
	keep := map[string]map[string]bool{ models.LibraryKindCollection: {} , models.LibraryKindItem: {} }
	for _ , pass := range []struct {
		kind    string
		phase   string
		records []zotio.Item
	}{ { models.LibraryKindCollection , "collections" , library.Collections } , { models.LibraryKindItem , "items" , library.Items } } {
		state.update( func( status *importStatus ) { status.Phase = pass.phase } )
		for start := 0; start < len( pass.records ); start += importBatch {
			batch := pass.records[ start:min( start + importBatch , len( pass.records ) ) ]
			inputs := make( []models.LibraryInput , 0 , len( batch ) )
			for _ , record := range batch {
				key := zotio.Str( record , "key" )
				version := int64( 0 )
				if number , ok := record[ "version" ].( int64 ); ok { version = number }
				if mode == "merge" { record , version = handlers.overlay( user_id , pass.kind , key , record , version ) }
				data , marshal_err := json.Marshal( record )
				if marshal_err != nil || len( data ) > models.LibraryMaxData {
					state.update( func( status *importStatus ) { status.Warnings = append( status.Warnings , "skipped " + key + ": too large" ) } )
					continue
				}
				keep[ pass.kind ][ key ] = true
				inputs = append( inputs , models.LibraryInput{ Key: key , Version: version , Data: data } )
			}
			results , upsert_err := models.UpsertLibrary( handlers.Store , user_id , pass.kind , inputs )
			if upsert_err != nil {
				fail( fmt.Errorf( "saving %s: %w" , pass.phase , upsert_err ) )
				return
			}
			changed := 0
			for _ , result := range results {
				if result.Changed { changed += 1 }
			}
			state.update( func( status *importStatus ) {
				status.Changed += changed
				status.Processed += len( batch )
			} )
		}
	}
	state.update( func( status *importStatus ) { status.Phase = "files" } )
	for _ , item := range library.Items {
		key := zotio.Str( item , "key" )
		files := library.Files[ key ]
		if len( files ) == 0 { continue }
		sources := make( []dav.SourceFile , 0 , len( files ) )
		for _ , file := range files {
			sources = append( sources , dav.SourceFile{ Name: file.Name , ModTime: file.ModTime , Open: file.Open } )
		}
		mtime := int64( 0 )
		if number , ok := item[ "mtime" ].( int64 ); ok { mtime = number }
		skipped , put_err := handlers.DAV.PutAttachment( user_id , key , zotio.Str( item , "filename" ) , sources , zotio.Str( item , "md5" ) , mtime )
		state.update( func( status *importStatus ) {
			status.Processed += 1
			switch {
			case put_err != nil:
				status.FilesFailed += 1
				if len( status.Warnings ) < 200 { status.Warnings = append( status.Warnings , "file for " + key + ": " + put_err.Error() ) }
			case skipped:
				status.FilesSkipped += 1
			default:
				status.Files += 1
			}
		} )
		if put_err != nil && ( errors.Is( put_err , dav.ErrDiskFull ) || strings.Contains( put_err.Error() , "is full" ) ) {
			state.update( func( status *importStatus ) { status.Warnings = append( status.Warnings , "stopped copying files: no room left" ) } )
			break
		}
	}
	if mode == "replace" {
		state.update( func( status *importStatus ) { status.Phase = "cleanup" } )
		for _ , kind := range []string{ models.LibraryKindItem , models.LibraryKindCollection } {
			records , list_err := models.ListLibrary( handlers.Store , user_id , kind )
			if list_err != nil {
				fail( list_err )
				return
			}
			for _ , record := range records {
				if keep[ kind ][ record.Key ] { continue }
				if _ , delete_err := models.DeleteLibraryRecord( handlers.Store , user_id , kind , record.Key ); delete_err == nil {
					state.update( func( status *importStatus ) { status.Deleted += 1 } )
				}
			}
		}
	}
	state.update( func( status *importStatus ) {
		status.Running , status.Done , status.Phase , status.FinishedAt = false , true , "done" , time.Now().UTC()
	} )
}

// overlay lays an imported record over what is stored under its key, for a
// merge: an export carries less than Zotero itself (no dateAdded, sync
// version or relations in RIS, say), and importing one should not drop what
// is already known. Fields the import has win; the stored version stays
// unless the import has its own.
func ( handlers *Handlers ) overlay( user_id uint64 , kind string , key string , record zotio.Item , version int64 ) ( merged zotio.Item , merged_version int64 ) {
	merged , merged_version = record , version
	stored , err := models.GetLibraryRecord( handlers.Store , user_id , kind , key )
	if err != nil || stored.Deleted { return }
	base := zotio.Item{}
	if json.Unmarshal( stored.Data , &base ) != nil { return }
	// A different item type means a different item: no overlay.
	if zotio.Str( base , "itemType" ) != zotio.Str( record , "itemType" ) && kind == models.LibraryKindItem { return }
	// Round trip the import through JSON, so numbers compare the way the
	// stored ones do.
	if raw , marshal_err := json.Marshal( record ); marshal_err == nil {
		fresh := zotio.Item{}
		if json.Unmarshal( raw , &fresh ) == nil { record = fresh }
	}
	for field , value := range record { base[ field ] = value }
	merged = base
	if version == 0 { merged_version = stored.Version }
	return
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

type exportRequest struct {
	libraryQuery
	Format     string `json:"format" query:"format" jsonschema:"enum=rdf,enum=ris,enum=csv" error:"format must be rdf, ris or csv"`
	Files      string `json:"files,omitempty" query:"files" jsonschema:"enum=true,enum=false" jsonschema_description:"true: a zip with each attachment's files under files/, as Zotero's Export Files makes."`
	Collection string `json:"collection,omitempty" query:"collection" jsonschema:"maxLength=8" jsonschema_description:"Only this collection and the ones inside it." error:"collection must be a Zotero key"`
}

// libraryExport writes your library as a file Zotero imports.
//
// @route("GET /api/library/export", summary="Export your library",
//     tag="Library", feature="webdav", errors=(404,), reply_type="application/octet-stream",
//     description="Zotero RDF or RIS (both import into Zotero, files included) or CSV (for spreadsheets). "
//         "With files=true it is a zip of the export and its files/ folder: unzip it and import the file "
//         "inside with Zotero's File > Import.",
// )
func ( handlers *Handlers ) libraryExport( c fiber.Ctx , in *exportRequest ) ( err error ) {
	format , found := zotio.FindFormat( in.Format )
	if found == false || format.Export == false {
		err = refuse( fiber.StatusBadRequest , zotio.ErrFormat.Error() )
		return
	}
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	library , err := handlers.exportLibrary( user_id , in.Collection )
	if err != nil { return }
	with_files := in.Files == "true"
	name := "Exported Items"
	if in.Collection != "" {
		for _ , collection := range library.Collections {
			if zotio.Str( collection , "key" ) == in.Collection { name = safeDownloadName( zotio.Str( collection , "name" ) ) }
		}
	}
	download := name + "." + format.Ext
	content_type := map[string]string{ "rdf": "application/rdf+xml" , "ris": "application/x-research-info-systems" , "csv": "text/csv" }[ format.ID ]
	if with_files {
		download , content_type = name + ".zip" , "application/zip"
	}
	c.Set( fiber.HeaderContentType , content_type )
	c.Set( fiber.HeaderContentDisposition , contentDisposition( download ) )
	c.Set( "Content-Security-Policy" , "sandbox; default-src 'none'" )
	c.Set( "X-Content-Type-Options" , "nosniff" )
	if c.Method() == fiber.MethodHead { return }
	connection := c.RequestCtx().Conn()
	c.RequestCtx().SetConnectionClose()
	c.Response().SetBodyStreamWriter( func( writer *bufio.Writer ) {
		defer func() {
			if recovered := recover(); recovered != nil { log.Printf( "[export] account %d: %v" , user_id , recovered ) }
		}()
		out := &deadlineWriter{ writer: writer , connection: connection }
		if export_err := zotio.Export( out , format , library , with_files , name ); export_err != nil {
			log.Printf( "[export] account %d: %v" , user_id , export_err )
		}
		writer.Flush()
	} )
	return
}

// deadlineWriter pushes the write deadline on with every write, so a long
// download is not cut off by the server's WriteTimeout while it keeps moving.
type deadlineWriter struct {
	writer     *bufio.Writer
	connection net.Conn
}

func ( out *deadlineWriter ) Write( data []byte ) ( written int , err error ) {
	out.connection.SetWriteDeadline( time.Now().Add( 60 * time.Second ) )
	written , err = out.writer.Write( data )
	return
}

func safeDownloadName( name string ) ( result string ) {
	result = strings.Map( func( r rune ) rune {
		if r < 0x20 || strings.ContainsRune( `/\:*?"<>|` , r ) { return '_' }
		return r
	} , strings.TrimSpace( name ) )
	if result == "" { result = "Exported Items" }
	return
}

// exportLibrary is the account's library as zotio wants it, only one
// collection's part when collection is set, with each attachment's files
// read from its zip as the export reaches them.
func ( handlers *Handlers ) exportLibrary( user_id uint64 , only string ) ( library *zotio.Library , err error ) {
	library = zotio.NewLibrary()
	collections , err := models.ListLibrary( handlers.Store , user_id , models.LibraryKindCollection )
	if err != nil { return }
	items , err := models.ListLibrary( handlers.Store , user_id , models.LibraryKindItem )
	if err != nil { return }
	decode := func( record *models.LibraryRecord ) ( data zotio.Item ) {
		data = zotio.Item{}
		if json.Unmarshal( record.Data , &data ) != nil { data = nil }
		if data != nil { data[ "key" ] = record.Key }
		return
	}
	wanted := map[string]bool{}
	if only != "" {
		if models.ZoteroKeyPattern.MatchString( only ) == false {
			err = refuse( fiber.StatusBadRequest , "collection must be a Zotero key" )
			return
		}
		parent := map[string]string{}
		for _ , record := range collections { parent[ record.Key ] = record.ParentKey }
		for _ , record := range collections {
			for key , depth := record.Key , 0; key != "" && depth < 100; key , depth = parent[ key ] , depth + 1 {
				if key == only {
					wanted[ record.Key ] = true
					break
				}
			}
		}
		if wanted[ only ] == false {
			err = refuse( fiber.StatusNotFound , "no such collection" )
			return
		}
	}
	for _ , record := range collections {
		if only != "" && wanted[ record.Key ] == false { continue }
		if data := decode( record ); data != nil {
			if only == record.Key { data[ "parentCollection" ] = false }
			library.Collections = append( library.Collections , data )
		}
	}
	chosen := map[string]bool{}
	for _ , record := range items {
		if record.ParentKey != "" { continue }
		if only == "" {
			chosen[ record.Key ] = true
			continue
		}
		for _ , key := range record.Collections {
			if wanted[ key ] { chosen[ record.Key ] = true }
		}
	}
	for _ , record := range items {
		if chosen[ record.Key ] == false && chosen[ record.ParentKey ] == false { continue }
		if data := decode( record ); data != nil { library.Items = append( library.Items , data ) }
	}
	files := map[string]bool{}
	if attachments , list_err := models.ListDAVAttachments( handlers.Store , user_id ); list_err == nil {
		for _ , attachment := range attachments { files[ attachment.Key ] = attachment.HasZip }
	}
	server := handlers.DAV
	library.Lookup = func( key string ) ( found []zotio.File ) {
		if files[ key ] == false { return }
		attachment , open_err := server.OpenAttachment( user_id , key )
		if open_err != nil { return }
		entries := attachment.Entries
		attachment.Close()
		for _ , entry := range entries {
			name := entry.Name
			found = append( found , zotio.File{ Name: name , Size: entry.Size , Open: func() ( io.ReadCloser , error ) {
				opened , open_err := server.OpenAttachment( user_id , key )
				if open_err != nil { return nil , open_err }
				reader , _ , entry_err := opened.OpenEntry( name )
				if entry_err != nil {
					opened.Close()
					return nil , entry_err
				}
				return &closeBoth{ reader: reader , also: opened } , nil
			} } )
		}
		return
	}
	return
}

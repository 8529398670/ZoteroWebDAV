package routes

import (
	json "encoding/json"
	fmt "fmt"
	mime "mime"
	os "os"
	path "path"
	strings "strings"
	sync "sync"
	time "time"
	io "io"

	fiber "github.com/gofiber/fiber/v3"

	dav "zoterowebdav/server/dav"
	models "zoterowebdav/server/models"
)

// Uploading one attachment's file: what turns an attachment that only links
// to a file somewhere (linked_file, as an import makes of a file its upload
// did not carry) into one whose file is stored here (imported_file), so
// every client syncs it. The file comes in pieces of ImportChunkBytes, the
// way an import's upload does; the last piece stores it as zotero/<KEY>.zip
// and .prop and rewrites the item.

// AttachmentUploadPath is the other route bodies of ImportChunkBytes may
// reach.
const AttachmentUploadPath = "/api/library/attachments/upload"

type attachmentUpload struct {
	path     string
	filename string
	size     int64
	mtime    int64
	pieces   map[int64]bool
	received int64
}

var attachmentUploads = struct {
	mu   sync.Mutex
	open map[string]*attachmentUpload
}{ open: map[string]*attachmentUpload{} }

type uploadAttachmentRequest struct {
	libraryQuery
	Key      string `json:"key" query:"key" jsonschema:"minLength=8,maxLength=8" jsonschema_description:"The attachment item's key." error:"key must be a Zotero key: 8 of A-Z and 2-9"`
	Filename string `json:"filename" query:"filename" jsonschema:"minLength=1,maxLength=255" jsonschema_description:"The file's name, without folders." error:"filename must be 1-255 characters"`
	Size     int64  `json:"size" query:"size" jsonschema:"minimum=1" jsonschema_description:"The whole file's size in bytes." error:"size must be positive"`
	Offset   int64  `json:"offset" query:"offset" jsonschema:"minimum=0" jsonschema_description:"Where this piece starts: a multiple of chunk_bytes (GET /api/library/formats). Offset 0 starts the upload over." error:"offset must be 0 or more"`
	MTime    int64  `json:"mtime,omitempty" query:"mtime" jsonschema:"minimum=0" jsonschema_description:"The file's modification time, in milliseconds since 1970. Default: now." error:"mtime must be 0 or more"`
}

type uploadAttachmentReply struct {
	Received int64  `json:"received"`
	Done     bool   `json:"done" jsonschema_description:"The whole file arrived and is stored; the item is an imported file now."`
	MD5      string `json:"md5,omitempty"`
}

// uploadAttachment takes one piece of an attachment's file.
//
// @route("POST /api/library/attachments/upload", summary="Upload an attachment's file",
//     tag="Library", feature="webdav", errors=(404, 409), body_type="application/octet-stream",
//     description="Stores the file of an attachment item, in pieces: each starts at offset, a multiple of "
//         "chunk_bytes, and all but the last are chunk_bytes long. When the last piece arrives the file is "
//         "stored in your WebDAV folder as Zotero's sync stores it (zotero/<KEY>.zip and .prop), and the item "
//         "becomes an imported_file named filename: an attachment that linked to a file elsewhere now syncs "
//         "it to every client.",
// )
func ( handlers *Handlers ) uploadAttachment( c fiber.Ctx , in *uploadAttachmentRequest ) ( reply *uploadAttachmentReply , err error ) {
	body := c.Body()
	filename := strings.TrimSpace( in.Filename )
	switch {
	case models.ZoteroKeyPattern.MatchString( in.Key ) == false:
		err = refuse( fiber.StatusBadRequest , models.ErrLibraryKey.Error() )
	case filename == "" || filename == "." || filename == ".." || strings.ContainsAny( filename , "/\\\x00" ):
		err = refuse( fiber.StatusBadRequest , "filename must be a file's name, without folders" )
	case len( body ) == 0 || len( body ) > ImportChunkBytes:
		err = refuse( fiber.StatusBadRequest , fmt.Sprintf( "a piece is 1 to %d bytes" , ImportChunkBytes ) )
	case in.Offset % ImportChunkBytes != 0:
		err = refuse( fiber.StatusBadRequest , fmt.Sprintf( "a piece starts at a multiple of %d" , ImportChunkBytes ) )
	case in.Offset + int64( len( body ) ) > in.Size:
		err = refuse( fiber.StatusBadRequest , "more than the size given" )
	case len( body ) != ImportChunkBytes && in.Offset + int64( len( body ) ) != in.Size:
		err = refuse( fiber.StatusBadRequest , fmt.Sprintf( "only the last piece may be shorter than %d bytes" , ImportChunkBytes ) )
	case in.Size > handlers.cfg().WebDAV.MaxFileBytes:
		err = refuse( fiber.StatusBadRequest , "the file is larger than this server takes" )
	case handlers.DAV == nil:
		err = refuse( fiber.StatusNotFound , "WebDAV is off" )
	}
	if err != nil { return }
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	record , get_err := models.GetLibraryRecord( handlers.Store , library_id , models.LibraryKindItem , in.Key )
	if get_err != nil || record.Deleted || record.ItemType != "attachment" {
		err = refuse( fiber.StatusNotFound , "no attachment item " + in.Key )
		return
	}

	id := fmt.Sprintf( "%d/%s" , library_id , in.Key )
	attachmentUploads.mu.Lock()
	upload := attachmentUploads.open[ id ]
	if in.Offset == 0 && ( upload == nil || upload.received > 0 ) {
		if upload != nil { os.Remove( upload.path ) }
		dir , dir_err := handlers.importDir()
		if dir_err != nil {
			attachmentUploads.mu.Unlock()
			err = dir_err
			return
		}
		file , create_err := os.CreateTemp( dir , fmt.Sprintf( "attachment-%d-*" , library_id ) )
		if create_err != nil {
			attachmentUploads.mu.Unlock()
			err = create_err
			return
		}
		file.Close()
		upload = &attachmentUpload{ path: file.Name() , filename: filename , size: in.Size , mtime: in.MTime , pieces: map[int64]bool{} }
		attachmentUploads.open[ id ] = upload
	}
	if upload == nil || upload.size != in.Size || upload.filename != filename {
		attachmentUploads.mu.Unlock()
		err = refuse( fiber.StatusConflict , "no upload of that file started: send offset 0 first" )
		return
	}
	if upload.pieces[ in.Offset ] == false {
		file , open_err := os.OpenFile( upload.path , os.O_WRONLY , 0o600 )
		if open_err == nil {
			_ , open_err = file.WriteAt( body , in.Offset )
			if close_err := file.Close(); open_err == nil { open_err = close_err }
		}
		if open_err != nil {
			attachmentUploads.mu.Unlock()
			err = open_err
			return
		}
		upload.pieces[ in.Offset ] = true
		upload.received += int64( len( body ) )
	}
	reply = &uploadAttachmentReply{ Received: upload.received }
	if upload.received < upload.size {
		attachmentUploads.mu.Unlock()
		return
	}
	delete( attachmentUploads.open , id )
	attachmentUploads.mu.Unlock()
	defer os.Remove( upload.path )

	modified := time.Now()
	if upload.mtime > 0 { modified = time.UnixMilli( upload.mtime ) }
	source := dav.SourceFile{ Name: upload.filename , ModTime: modified , Open: func() ( io.ReadCloser , error ) { return os.Open( upload.path ) } }
	if _ , err = handlers.DAV.PutAttachment( library_id , in.Key , upload.filename , []dav.SourceFile{ source } , "" , modified.UnixMilli() ); err != nil { return }
	stored , err := models.GetDAVAttachment( handlers.Store , library_id , in.Key )
	if err != nil { return }

	data := map[string]any{}
	if err = json.Unmarshal( record.Data , &data ); err != nil { return }
	data[ "linkMode" ] = "imported_file"
	data[ "filename" ] = upload.filename
	delete( data , "path" )
	if content_type , _ := data[ "contentType" ].( string ); content_type == "" {
		if guessed := mime.TypeByExtension( strings.ToLower( path.Ext( upload.filename ) ) ); guessed != "" {
			data[ "contentType" ] = strings.TrimSpace( strings.SplitN( guessed , ";" , 2 )[ 0 ] )
		}
	}
	raw , err := json.Marshal( data )
	if err != nil { return }
	if _ , err = models.UpsertLibrary( handlers.Store , library_id , models.LibraryKindItem , []models.LibraryInput{ { Key: in.Key , Version: record.Version , Data: raw } } ); err != nil { return }
	reply.Done , reply.MD5 = true , stored.MD5
	return
}

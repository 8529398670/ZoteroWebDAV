package routes

import (
	json "encoding/json"
	errors "errors"
	io "io"
	strings "strings"
	time "time"

	fiber "github.com/gofiber/fiber/v3"

	apidoc "zoterowebdav/server/apidoc"
	db "zoterowebdav/server/db"
	models "zoterowebdav/server/models"
	zotio "zoterowebdav/server/zotio"
)

// The library API: the metadata that goes with the files in your WebDAV
// folder, and the files themselves, straight out of Zotero's zips.
//
// Zotero's WebDAV sync carries files only. A client that has the metadata
// (PRMA, reading zotero.sqlite beside Zotero) pushes items and collections
// here, as Zotero's own JSON; a client with no Zotero beside it pulls them
// back with GET /api/library/changes?since=<rev>, which hands out only what
// changed, tombstones included, oldest change first. POST
// /api/library/attachments/check says which attachment files the folder
// already has, so a client copies only what is missing.
//
// Everything is one of the caller's own libraries: the primary, or the one
// named by the library query parameter (its WebDAV name). An API key works here, at its
// owner's account: that is how a program uses it.

// ---------------------------------------------------------------------------
// items and collections
// ---------------------------------------------------------------------------

type itemKeyPath struct {
	libraryQuery
	ItemKey string `json:"item_key" path:"item_key" jsonschema:"minLength=8,maxLength=8" jsonschema_description:"The Zotero item key: 8 of A-Z and 0-9." error:"item_key must be a Zotero key: 8 of A-Z and 0-9"`
}

type collectionKeyPath struct {
	libraryQuery
	CollectionKey string `json:"collection_key" path:"collection_key" jsonschema:"minLength=8,maxLength=8" jsonschema_description:"The Zotero collection key: 8 of A-Z and 0-9." error:"collection_key must be a Zotero key: 8 of A-Z and 0-9"`
}

type libraryBody struct {
	Version int64          `json:"version,omitempty" jsonschema:"minimum=0" jsonschema_description:"Zotero's version of it, if you have one. Stored and handed back as it is." error:"version must be 0 or more"`
	Data    map[string]any `json:"data" jsonschema_description:"Zotero's JSON for it: the data object of the Zotero web API, or the same fields read from zotero.sqlite. At most 256 KB." error:"data must be a JSON object"`
}

type putItemRequest struct {
	itemKeyPath
	libraryBody
}

type putCollectionRequest struct {
	collectionKeyPath
	libraryBody
}

type libraryPutReply struct {
	Key     string `json:"key"`
	Rev     uint64 `json:"rev" jsonschema_description:"Its change number."`
	Changed bool   `json:"changed" jsonschema_description:"false when it was the same as what was stored, which then keeps its change number."`
}

// libraryRow is a record as lists show it, without its data.
type libraryRow struct {
	Kind        string    `json:"kind" jsonschema:"enum=item,enum=collection"`
	Key         string    `json:"key"`
	Version     int64     `json:"version"`
	Rev         uint64    `json:"rev"`
	Deleted     bool      `json:"deleted,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	Title       string    `json:"title,omitempty"`
	ItemType    string    `json:"item_type,omitempty"`
	ParentKey   string    `json:"parent_key,omitempty"`
	Creators    string    `json:"creators,omitempty"`
	Year        string    `json:"year,omitempty"`
	DOI         string    `json:"doi,omitempty"`
	Modified    string    `json:"modified,omitempty"`
	Collections []string  `json:"collections,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	HasFile     bool      `json:"has_file,omitempty" jsonschema_description:"Whether your WebDAV folder has the file: an attachment's own zotero/<KEY>.zip, or for a regular item one of its attachments'."`
	FileKey     string    `json:"file_key,omitempty" jsonschema_description:"The attachment key whose file to fetch (GET /api/library/attachments/{file_key}/file): the item's own, or its first attachment with a file, a PDF first."`
	FileType    string    `json:"file_type,omitempty" jsonschema_description:"The content type of file_key's attachment, as its item says: application/pdf for a PDF."`
}

func ( libraryRow ) APIName() ( name string ) { name = "LibraryRecord"; return }

// libraryRecordReply is one record with its data.
type libraryRecordReply struct {
	libraryRow
	Data map[string]any `json:"data,omitempty"`
}

func ( libraryRecordReply ) APIName() ( name string ) { name = "LibraryRecordWithData"; return }

func newLibraryRow( record *models.LibraryRecord ) ( row libraryRow ) {
	row = libraryRow{
		Kind: record.Kind , Key: record.Key , Version: record.Version , Rev: record.Rev , Deleted: record.Deleted ,
		UpdatedAt: record.UpdatedAt , Title: record.Title , ItemType: record.ItemType , ParentKey: record.ParentKey ,
		Creators: record.Creators , Year: record.Year , DOI: record.DOI , Modified: record.Modified ,
		Collections: record.Collections , Filename: record.Filename , ContentType: record.ContentType ,
	}
	return
}

func newLibraryRecordReply( record *models.LibraryRecord ) ( reply libraryRecordReply , err error ) {
	reply = libraryRecordReply{ libraryRow: newLibraryRow( record ) }
	if len( record.Data ) > 0 {
		err = json.Unmarshal( record.Data , &reply.Data )
	}
	return
}

func ( handlers *Handlers ) upsertLibrary( c fiber.Ctx , library string , kind string , key string , body libraryBody ) ( reply *libraryPutReply , err error ) {
	library_id , err := handlers.libraryOf( c , library )
	if err != nil { return }
	if models.ZoteroKeyPattern.MatchString( key ) == false {
		err = refuse( fiber.StatusBadRequest , models.ErrLibraryKey.Error() )
		return
	}
	data , err := json.Marshal( body.Data )
	if err != nil { return }
	results , put_err := models.UpsertLibrary( handlers.Store , library_id , kind , []models.LibraryInput{ { Key: key , Version: body.Version , Data: data } } )
	switch {
	case errors.Is( put_err , models.ErrLibraryKey ) , errors.Is( put_err , models.ErrLibraryData ):
		err = refuse( fiber.StatusBadRequest , put_err.Error() )
		return
	case put_err != nil:
		err = put_err
		return
	}
	reply = &libraryPutReply{ Key: results[ 0 ].Key , Rev: results[ 0 ].Rev , Changed: results[ 0 ].Changed }
	return
}

// putLibraryItem stores one item. bulk=True gives
// POST /api/bulk/library/items, up to 100 at once.
//
// @route("POST /api/library/items/{item_key}", summary="Store an item",
//     tag="Library", feature="webdav",
//     description="Creates or replaces one item: Zotero's JSON for it under data. The same version and data "
//         "as already stored changes nothing, so a client can push its whole library again cheaply. "
//         "POST /api/bulk/library/items takes up to 100.",
//     bulk=True,
// )
func ( handlers *Handlers ) putLibraryItem( c fiber.Ctx , in *putItemRequest ) ( reply *libraryPutReply , err error ) {
	reply , err = handlers.upsertLibrary( c , in.Library , models.LibraryKindItem , in.ItemKey , in.libraryBody )
	return
}

// putLibraryCollection stores one collection.
//
// @route("POST /api/library/collections/{collection_key}", summary="Store a collection",
//     tag="Library", feature="webdav",
//     description="Creates or replaces one collection: Zotero's JSON for it under data. "
//         "POST /api/bulk/library/collections takes up to 100.",
//     bulk=True,
// )
func ( handlers *Handlers ) putLibraryCollection( c fiber.Ctx , in *putCollectionRequest ) ( reply *libraryPutReply , err error ) {
	reply , err = handlers.upsertLibrary( c , in.Library , models.LibraryKindCollection , in.CollectionKey , in.libraryBody )
	return
}

func ( handlers *Handlers ) deleteLibrary( c fiber.Ctx , library string , kind string , key string ) ( reply *libraryPutReply , err error ) {
	library_id , err := handlers.libraryOf( c , library )
	if err != nil { return }
	rev , delete_err := models.DeleteLibraryRecord( handlers.Store , library_id , kind , key )
	switch {
	case errors.Is( delete_err , db.ErrNotFound ):
		err = refuse( fiber.StatusNotFound , "no such " + kind )
		return
	case delete_err != nil:
		err = delete_err
		return
	}
	reply = &libraryPutReply{ Key: key , Rev: rev , Changed: true }
	return
}

// deleteLibraryItem leaves a tombstone, so clients pulling changes hear of it.
//
// @route("POST /api/library/items/{item_key}/delete", summary="Delete an item",
//     tag="Library", feature="webdav", errors=(404,),
//     description="The item becomes a tombstone, which GET /api/library/changes hands out for "
//         "webdav.tombstone_days. Its attachment file, if any, stays in your WebDAV folder. "
//         "children=true deletes its attachments and notes with it and answers what was removed.",
//     bulk=True,
// )
func ( handlers *Handlers ) deleteLibraryItem( c fiber.Ctx , in *deleteItemRequest ) ( reply *deleteItemReply , err error ) {
	if in.Children != "true" {
		put , delete_err := handlers.deleteLibrary( c , in.Library , models.LibraryKindItem , in.ItemKey )
		if delete_err != nil {
			err = delete_err
			return
		}
		reply = &deleteItemReply{ libraryPutReply: *put }
		return
	}
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	removed , delete_err := models.DeleteLibraryItemTree( handlers.Store , library_id , in.ItemKey )
	switch {
	case errors.Is( delete_err , db.ErrNotFound ):
		err = refuse( fiber.StatusNotFound , "no such item" )
		return
	case delete_err != nil:
		err = delete_err
		return
	}
	reply = &deleteItemReply{ libraryPutReply: libraryPutReply{ Key: in.ItemKey , Changed: true } , Removed: []removedRecord{} }
	for _ , record := range removed {
		built := removedRecord{ Key: record.Key , Version: record.Version }
		if len( record.Data ) > 0 {
			if err = json.Unmarshal( record.Data , &built.Data ); err != nil { return }
		}
		reply.Removed = append( reply.Removed , built )
	}
	if stored , get_err := models.GetLibraryRecord( handlers.Store , library_id , models.LibraryKindItem , in.ItemKey ); get_err == nil {
		reply.Rev = stored.Rev
	}
	return
}

type deleteItemRequest struct {
	itemKeyPath
	Children string `json:"children,omitempty" query:"children" jsonschema:"enum=true" jsonschema_description:"true deletes its child items (attachments and notes) with it, as Zotero does, and answers removed."`
}

type removedRecord struct {
	Key     string         `json:"key"`
	Version int64          `json:"version"`
	Data    map[string]any `json:"data,omitempty"`
}

type deleteItemReply struct {
	libraryPutReply
	Removed []removedRecord `json:"removed,omitempty" jsonschema_description:"With children=true: every item deleted, the item first, as it was, so it can be put back with POST /api/bulk/library/items."`
}

// deleteLibraryCollection leaves a tombstone too.
//
// @route("POST /api/library/collections/{collection_key}/delete", summary="Delete a collection",
//     tag="Library", feature="webdav", errors=(404,),
//     bulk=True,
// )
func ( handlers *Handlers ) deleteLibraryCollection( c fiber.Ctx , in *collectionKeyPath ) ( reply *libraryPutReply , err error ) {
	reply , err = handlers.deleteLibrary( c , in.Library , models.LibraryKindCollection , in.CollectionKey )
	return
}

// getLibraryItem is one item with its data.
//
// @route("GET /api/library/items/{item_key}", summary="One item",
//     tag="Library", feature="webdav", errors=(404,),
// )
func ( handlers *Handlers ) getLibraryItem( c fiber.Ctx , in *itemKeyPath ) ( reply *libraryRecordReply , err error ) {
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	record , get_err := models.GetLibraryRecord( handlers.Store , user_id , models.LibraryKindItem , in.ItemKey )
	if errors.Is( get_err , db.ErrNotFound ) || ( get_err == nil && record.Deleted ) {
		err = refuse( fiber.StatusNotFound , "no such item" )
		return
	}
	if get_err != nil {
		err = get_err
		return
	}
	built , err := newLibraryRecordReply( record )
	if err != nil { return }
	if attachment , attachment_err := models.GetDAVAttachment( handlers.Store , user_id , record.Key ); attachment_err == nil {
		built.HasFile = attachment.HasZip
	}
	reply = &built
	return
}

type listLibraryRequest struct {
	libraryQuery
	Page
	Q    string `json:"q,omitempty" query:"q" jsonschema_description:"Keep records whose title, creators, year, DOI or key contains this, in any case."`
	Top  string `json:"top,omitempty" query:"top" jsonschema:"enum=true" jsonschema_description:"Leave out child items (attachments and notes) and sub-collections."`
	Sort string `json:"sort,omitempty" query:"sort" jsonschema:"enum=title,enum=creators,enum=year,enum=modified,enum=item_type" jsonschema_description:"Field to sort the whole list by. Default modified, newest first."`
	Direction
}

type libraryListReply struct {
	Records []libraryRow `json:"records"`
	Rev     uint64       `json:"rev" jsonschema_description:"Your library's last change number."`
	Paged
}

func ( handlers *Handlers ) listLibrary( c fiber.Ctx , kind string , in *listLibraryRequest ) ( reply *libraryListReply , err error ) {
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	records , err := models.ListLibrary( handlers.Store , user_id , kind )
	if err != nil { return }
	files := map[string]bool{}
	if kind == models.LibraryKindItem {
		attachments , list_err := models.ListDAVAttachments( handlers.Store , user_id )
		if list_err != nil {
			err = list_err
			return
		}
		for _ , attachment := range attachments { files[ attachment.Key ] = attachment.HasZip }
	}
	// A paper's file is on its child attachment item: the parent points at
	// the first child with a file, a PDF before anything else.
	child_file := map[string]string{}
	types := map[string]string{}
	for _ , record := range records {
		types[ record.Key ] = record.ContentType
		if record.ParentKey == "" || files[ record.Key ] == false { continue }
		current , found := child_file[ record.ParentKey ]
		if found == false || ( record.ContentType == "application/pdf" && current == "" ) {
			child_file[ record.ParentKey ] = record.Key
		}
	}
	rows := make( []libraryRow , 0 , len( records ) )
	for _ , record := range records {
		if in.Top == "true" && record.ParentKey != "" { continue }
		row := newLibraryRow( record )
		if files[ record.Key ] {
			row.HasFile , row.FileKey = true , record.Key
		} else if key := child_file[ record.Key ]; key != "" {
			row.HasFile , row.FileKey = true , key
		}
		if row.HasFile { row.FileType = types[ row.FileKey ] }
		rows = append( rows , row )
	}
	rows = matchRows( rows , in.Q , "title" , "creators" , "year" , "doi" , "key" )
	sort , direction := in.Sort , in.Direction
	if sort == "" {
		sort , direction = "modified" , Direction{ Dir: "desc" }
	}
	reply = &libraryListReply{}
	if reply.Rev , _ , err = models.LibraryRev( handlers.Store , user_id ); err != nil { return }
	reply.Records , reply.Paged = sortedPage( rows , sort , direction , in.Page )
	return
}

// listLibraryItems is your items, for a screen to search and page through.
//
// @route("GET /api/library/items", summary="Your items",
//     tag="Library", feature="webdav",
//     description="Live items without their data, newest change first unless sort says otherwise. "
//         "has_file says whether your WebDAV folder has an attachment's file.",
// )
func ( handlers *Handlers ) listLibraryItems( c fiber.Ctx , in *listLibraryRequest ) ( reply *libraryListReply , err error ) {
	reply , err = handlers.listLibrary( c , models.LibraryKindItem , in )
	return
}

// listLibraryCollections is your collections.
//
// @route("GET /api/library/collections", summary="Your collections",
//     tag="Library", feature="webdav",
// )
func ( handlers *Handlers ) listLibraryCollections( c fiber.Ctx , in *listLibraryRequest ) ( reply *libraryListReply , err error ) {
	reply , err = handlers.listLibrary( c , models.LibraryKindCollection , in )
	return
}

type libraryChangesRequest struct {
	libraryQuery
	Since uint64 `json:"since,omitempty" query:"since" jsonschema:"minimum=0" jsonschema_description:"The rev of the last change you have. 0 for everything." error:"since must be 0 or more"`
	Limit int    `json:"limit,omitempty" query:"limit" jsonschema:"minimum=1,maximum=1000,default=500" jsonschema_description:"Records to return. Default 500." error:"limit must be 1-1000"`
}

type libraryChangesReply struct {
	Records    []libraryRecordReply `json:"records" jsonschema_description:"Changed items and collections, with their data, oldest change first. Deleted ones have deleted: true and no data."`
	Rev        uint64               `json:"rev" jsonschema_description:"Pass this as since next time: the last change in records, or the library's last change once there are no more."`
	More       bool                 `json:"more" jsonschema_description:"true when there are more changes after these: ask again with since=rev."`
	FullResync bool                 `json:"full_resync" jsonschema_description:"true when since is older than the tombstones kept, so a delete may have been missed: drop what you have and read from since=0."`
}

// libraryChanges hands a client what changed since it last asked.
//
// @route("GET /api/library/changes", summary="What changed in your library",
//     tag="Library", feature="webdav",
//     description="Items and collections changed after since, with their data, oldest change first, deletes "
//         "included as tombstones. Ask again with since=rev while more is true. A since older than the "
//         "tombstones kept answers full_resync: true.",
// )
func ( handlers *Handlers ) libraryChanges( c fiber.Ctx , in *libraryChangesRequest ) ( reply *libraryChangesReply , err error ) {
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	limit := in.Limit
	if limit == 0 { limit = 500 }
	last , purged , err := models.LibraryRev( handlers.Store , user_id )
	if err != nil { return }
	reply = &libraryChangesReply{ Records: []libraryRecordReply{} , Rev: last }
	if in.Since > 0 && in.Since < purged {
		reply.FullResync = true
		return
	}
	records , more , err := models.LibraryChanges( handlers.Store , user_id , in.Since , limit )
	if err != nil { return }
	for _ , record := range records {
		built , build_err := newLibraryRecordReply( record )
		if build_err != nil {
			err = build_err
			return
		}
		reply.Records = append( reply.Records , built )
	}
	reply.More = more
	if more && len( records ) > 0 { reply.Rev = records[ len( records ) - 1 ].Rev }
	return
}

// ---------------------------------------------------------------------------
// attachments
// ---------------------------------------------------------------------------

type attachmentRow struct {
	Key       string    `json:"key"`
	MTime     int64     `json:"mtime,omitempty" jsonschema_description:"From the .prop: the main file's mtime, in ms."`
	MD5       string    `json:"md5,omitempty" jsonschema_description:"From the .prop: the main file's md5."`
	HasProp   bool      `json:"has_prop"`
	ZipSize   int64     `json:"zip_size,omitempty"`
	HasZip    bool      `json:"has_zip"`
	UpdatedAt time.Time `json:"updated_at"`
}

func ( attachmentRow ) APIName() ( name string ) { name = "Attachment"; return }

type attachmentsReply struct {
	Attachments []attachmentRow `json:"attachments"`
	Paged
}

type listAttachmentsRequest struct {
	libraryQuery
	Page
}

// listAttachments is the attachment files your WebDAV folder holds, as the
// zotero/<KEY>.prop files describe them.
//
// @route("GET /api/library/attachments", summary="Your attachment files",
//     tag="Library", feature="webdav",
//     description="Every attachment in your WebDAV folder's zotero/, by key: whether its .zip and .prop are "
//         "there, and the mtime and md5 the .prop gives.",
// )
func ( handlers *Handlers ) listAttachments( c fiber.Ctx , in *listAttachmentsRequest ) ( reply *attachmentsReply , err error ) {
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	rows , err := models.ListDAVAttachments( handlers.Store , library_id )
	if err != nil { return }
	built := make( []attachmentRow , 0 , len( rows ) )
	for _ , row := range rows {
		built = append( built , attachmentRow( *row ) )
	}
	reply = &attachmentsReply{}
	reply.Attachments , reply.Paged = cutPage( built , in.Page )
	return
}

type attachmentCheck struct {
	Key string `json:"key" jsonschema:"minLength=8,maxLength=8" error:"key must be a Zotero key: 8 of A-Z and 0-9"`
	MD5 string `json:"md5,omitempty" jsonschema:"maxLength=32" jsonschema_description:"The md5 of the main file you have, to tell a different file from the same one." error:"md5 must be 32 hex characters"`
}

type checkAttachmentsRequest struct {
	libraryQuery
	Attachments []attachmentCheck `json:"attachments" jsonschema:"minItems=1,maxItems=1000" error:"attachments must list 1-1000 keys"`
}

type checkAttachmentsReply struct {
	Present   []string `json:"present" jsonschema_description:"The .zip and .prop are there, with the md5 you gave if you gave one."`
	Missing   []string `json:"missing" jsonschema_description:"No complete file: copy it (the .zip, then the .prop)."`
	Different []string `json:"different" jsonschema_description:"There, with a different md5."`
}

// checkAttachments says which attachment files a client still has to copy.
//
// @route("POST /api/library/attachments/check", summary="Which attachment files are missing",
//     tag="Library", feature="webdav",
//     description="For up to 1000 keys, whether your WebDAV folder has each attachment's file. A file counts "
//         "only with both its .zip and its .prop, since Zotero writes the .prop last.",
// )
func ( handlers *Handlers ) checkAttachments( c fiber.Ctx , in *checkAttachmentsRequest ) ( reply *checkAttachmentsReply , err error ) {
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	rows , err := models.ListDAVAttachments( handlers.Store , library_id )
	if err != nil { return }
	by_key := map[string]*models.DAVAttachment{}
	for _ , row := range rows { by_key[ row.Key ] = row }
	reply = &checkAttachmentsReply{ Present: []string{} , Missing: []string{} , Different: []string{} }
	for _ , wanted := range in.Attachments {
		if models.ZoteroKeyPattern.MatchString( wanted.Key ) == false {
			err = refuse( fiber.StatusBadRequest , "key must be a Zotero key: 8 of A-Z and 0-9" )
			return
		}
		row := by_key[ wanted.Key ]
		switch {
		case row == nil || row.HasZip == false || row.HasProp == false:
			reply.Missing = append( reply.Missing , wanted.Key )
		case wanted.MD5 != "" && strings.ToLower( wanted.MD5 ) != row.MD5:
			reply.Different = append( reply.Different , wanted.Key )
		default:
			reply.Present = append( reply.Present , wanted.Key )
		}
	}
	return
}

type attachmentFilesReply struct {
	Key   string              `json:"key"`
	Main  string              `json:"main" jsonschema_description:"The file GET .../file hands out when no name is given."`
	Files []attachmentFileRow `json:"files"`
}

type attachmentFileRow struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// preferredName is the attachment item's filename, when a client pushed it.
func ( handlers *Handlers ) preferredName( user_id uint64 , key string ) ( name string ) {
	if record , err := models.GetLibraryRecord( handlers.Store , user_id , models.LibraryKindItem , key ); err == nil {
		name = record.Filename
	}
	return
}

// listAttachmentFiles is what one attachment's zip holds.
//
// @route("GET /api/library/attachments/{item_key}/files", summary="The files in one attachment",
//     tag="Library", feature="webdav", errors=(404,),
// )
func ( handlers *Handlers ) listAttachmentFiles( c fiber.Ctx , in *itemKeyPath ) ( reply *attachmentFilesReply , err error ) {
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	attachment , open_err := handlers.DAV.OpenAttachment( user_id , in.ItemKey )
	if open_err != nil {
		err = refuse( fiber.StatusNotFound , "no file for that attachment" )
		return
	}
	defer attachment.Close()
	reply = &attachmentFilesReply{ Key: in.ItemKey , Main: attachment.Main( handlers.preferredName( user_id , in.ItemKey ) ) , Files: []attachmentFileRow{} }
	for _ , entry := range attachment.Entries {
		reply.Files = append( reply.Files , attachmentFileRow( entry ) )
	}
	return
}

type attachmentFileRequest struct {
	itemKeyPath
	Name   string `json:"name,omitempty" query:"name" jsonschema:"maxLength=1024" jsonschema_description:"Which file in the zip. Default: the main one." error:"name must be at most 1024 characters"`
	Inline string `json:"inline,omitempty" query:"inline" jsonschema:"enum=true" jsonschema_description:"true shows a PDF in the browser instead of downloading it. Other files still download."`
}

// getAttachmentFile streams one file out of an attachment's zip,
// decompressed as it goes: the PDF, usually.
//
// @route("GET /api/library/attachments/{item_key}/file", summary="One attachment's file",
//     tag="Library", feature="webdav", errors=(404,), reply_type="application/octet-stream",
//     description="Streams a file out of zotero/<KEY>.zip in your WebDAV folder, with nothing unpacked on "
//         "disk: the main file (the item's filename, the only file, or the largest), or the one name says.",
// )
func ( handlers *Handlers ) getAttachmentFile( c fiber.Ctx , in *attachmentFileRequest ) ( err error ) {
	user_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	attachment , open_err := handlers.DAV.OpenAttachment( user_id , in.ItemKey )
	if open_err != nil {
		err = refuse( fiber.StatusNotFound , "no file for that attachment" )
		return
	}
	name := in.Name
	if name == "" { name = attachment.Main( handlers.preferredName( user_id , in.ItemKey ) ) }
	reader , size , entry_err := attachment.OpenEntry( name )
	if entry_err != nil {
		attachment.Close()
		err = refuse( fiber.StatusNotFound , "no such file in that attachment" )
		return
	}
	stream := &closeBoth{ reader: reader , also: attachment }
	if in.Inline == "true" && strings.HasSuffix( strings.ToLower( name ) , ".pdf" ) {
		err = sendInlinePDF( c , stream , size , baseName( name ) )
		return
	}
	err = sendDownload( c , stream , size , baseName( name ) , time.Time{} )
	return
}

// sendInlinePDF lets the browser show a PDF in its own viewer. Only a PDF:
// any other file (an HTML snapshot, say) would run on this origin, so it
// downloads. Chrome's viewer draws the PDF in a frame of the document, and
// any Content-Security-Policy on it (even frame-ancestors alone),
// X-Frame-Options, or the cross-origin isolation headers leave the page blank,
// so this response drops the ones every other response carries. What keeps it safe is the type: application/pdf
// with nosniff is never read as HTML, and the viewer runs apart from the page.
var inlinePDFDropped = []string{
	"Content-Security-Policy" , fiber.HeaderXFrameOptions ,
	"Cross-Origin-Embedder-Policy" , "Cross-Origin-Resource-Policy" , "Cross-Origin-Opener-Policy" ,
}

func sendInlinePDF( c fiber.Ctx , reader io.ReadCloser , size int64 , name string ) ( err error ) {
	for _ , header := range inlinePDFDropped { c.Response().Header.Del( header ) }
	c.Set( fiber.HeaderContentType , "application/pdf" )
	c.Set( fiber.HeaderContentDisposition , "inline" + strings.TrimPrefix( contentDisposition( name ) , "attachment" ) )
	c.Set( "X-Content-Type-Options" , "nosniff" )
	err = c.SendStream( reader , int( size ) )
	return
}

// closeBoth closes an entry and the zip it came from.
type closeBoth struct {
	reader io.ReadCloser
	also   io.Closer
}

func ( both *closeBoth ) Read( data []byte ) ( read int , err error ) {
	read , err = both.reader.Read( data )
	return
}

func ( both *closeBoth ) Close() ( err error ) {
	err = errors.Join( both.reader.Close() , both.also.Close() )
	return
}

func baseName( name string ) ( base string ) {
	base = name[ strings.LastIndex( name , "/" ) + 1: ]
	return
}

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

type librarySchemaReply struct {
	ItemTypes    map[string][]string `json:"item_types" jsonschema_description:"Each Zotero item type's fields, in Zotero's order."`
	CreatorTypes map[string][]string `json:"creator_types" jsonschema_description:"Each item type's creator types, the main one first."`
}

// getLibrarySchema is Zotero's item types, their fields and creator types,
// for a screen that edits items.
//
// @route("GET /api/library/schema", summary="Zotero's item fields",
//     tag="Library", feature="webdav",
// )
func ( handlers *Handlers ) getLibrarySchema( c fiber.Ctx , in *apidoc.None ) ( reply *librarySchemaReply , err error ) {
	reply = &librarySchemaReply{ ItemTypes: zotio.TypeFields() , CreatorTypes: zotio.CreatorTypes() }
	return
}

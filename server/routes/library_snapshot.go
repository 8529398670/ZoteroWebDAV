package routes

import (
	bytes "bytes"
	gzip "compress/gzip"
	json "encoding/json"
	strconv "strconv"
	strings "strings"

	fiber "github.com/gofiber/fiber/v3"

	models "zoterowebdav/server/models"
)

// The whole library in one answer, for a client starting from nothing (or
// from a cache it has lost): every live item and collection with its data,
// and the attachment index, gzipped when the client takes it. The client
// then keeps up with GET /api/library/changes?since=rev and
// GET /api/library/attachments?since=, rather than paging through every
// change since 0.

type librarySnapshotRequest struct {
	libraryQuery
}

type librarySnapshot struct {
	Rev         uint64               `json:"rev"`
	FilesStamp  int64                `json:"files_stamp"`
	Items       []libraryRecordReply `json:"items"`
	Collections []libraryRecordReply `json:"collections"`
	Attachments []attachmentRow      `json:"attachments"`
}

// librarySnapshot answers the whole library at once.
//
// @route("GET /api/library/snapshot", summary="Your whole library at once",
//     tag="Library", feature="webdav", reply_type="application/json",
//     description="Every live item and collection with its data, and the attachment index, as of rev and "
//         "files_stamp: {rev, files_stamp, items, collections, attachments}. Gzipped when you send "
//         "Accept-Encoding: gzip. The ETag is rev and files_stamp, so If-None-Match answers 304 while nothing "
//         "changed. Then follow GET /api/library/changes?since=rev; a record changed while this was read may "
//         "come again there, which is harmless.",
// )
func ( handlers *Handlers ) librarySnapshot( c fiber.Ctx , in *librarySnapshotRequest ) ( err error ) {
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	// Read first: anything written while the lists below are read has a
	// later rev, so the client sees it again from changes, never misses it.
	stamp := models.LibraryFilesStamp( library_id )
	rev , _ , err := models.LibraryRev( handlers.Store , library_id )
	if err != nil { return }
	tag := `"` + strconv.FormatUint( rev , 10 ) + "-" + strconv.FormatInt( stamp , 10 ) + `"`
	c.Set( fiber.HeaderETag , tag )
	c.Set( fiber.HeaderCacheControl , "private, no-cache" )
	c.Set( fiber.HeaderVary , fiber.HeaderAcceptEncoding )
	if c.Get( fiber.HeaderIfNoneMatch ) == tag {
		err = c.SendStatus( fiber.StatusNotModified )
		return
	}
	snapshot := &librarySnapshot{ Rev: rev , FilesStamp: stamp }
	if snapshot.Items , err = handlers.snapshotRecords( library_id , models.LibraryKindItem ); err != nil { return }
	if snapshot.Collections , err = handlers.snapshotRecords( library_id , models.LibraryKindCollection ); err != nil { return }
	rows , err := models.ListDAVAttachments( handlers.Store , library_id )
	if err != nil { return }
	snapshot.Attachments = make( []attachmentRow , 0 , len( rows ) )
	for _ , row := range rows { snapshot.Attachments = append( snapshot.Attachments , attachmentRow( *row ) ) }
	body , err := json.Marshal( snapshot )
	if err != nil { return }
	c.Set( fiber.HeaderContentType , fiber.MIMEApplicationJSONCharsetUTF8 )
	if acceptsGzip( c.Get( fiber.HeaderAcceptEncoding ) ) {
		packed := &bytes.Buffer{}
		writer , _ := gzip.NewWriterLevel( packed , gzip.BestSpeed )
		writer.Write( body )
		if err = writer.Close(); err != nil { return }
		c.Set( fiber.HeaderContentEncoding , "gzip" )
		body = packed.Bytes()
	}
	err = c.Send( body )
	return
}

func ( handlers *Handlers ) snapshotRecords( library_id uint64 , kind string ) ( replies []libraryRecordReply , err error ) {
	records , err := models.ListLibrary( handlers.Store , library_id , kind )
	if err != nil { return }
	replies = make( []libraryRecordReply , 0 , len( records ) )
	for _ , record := range records {
		built , build_err := newLibraryRecordReply( record )
		if build_err != nil {
			err = build_err
			return
		}
		replies = append( replies , built )
	}
	return
}

// acceptsGzip reads Accept-Encoding: gzip listed, and not refused with q=0.
func acceptsGzip( header string ) ( ok bool ) {
	for _ , part := range strings.Split( header , "," ) {
		fields := strings.Split( strings.TrimSpace( part ) , ";" )
		if strings.EqualFold( strings.TrimSpace( fields[ 0 ] ) , "gzip" ) == false { continue }
		ok = true
		for _ , param := range fields[ 1: ] {
			if value , found := strings.CutPrefix( strings.ReplaceAll( param , " " , "" ) , "q=" ); found {
				if q , parse_err := strconv.ParseFloat( value , 64 ); parse_err == nil && q == 0 { ok = false }
			}
		}
	}
	return
}

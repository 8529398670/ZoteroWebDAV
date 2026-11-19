package routes

import (
	gzip "compress/gzip"
	json "encoding/json"
	io "io"
	http "net/http"
	strconv "strconv"
	testing "testing"
	time "time"

	fiber "github.com/gofiber/fiber/v3"

	models "zoterowebdav/server/models"
)

// raw is a GET with the session's cookie and these headers, answered as is.
func ( client *browser ) raw( t *testing.T , path string , headers map[string]string ) ( response *http.Response , body []byte ) {
	t.Helper()
	request , _ := http.NewRequest( http.MethodGet , path , nil )
	request.Header.Set( "Cookie" , client.cookie )
	for name , value := range headers { request.Header.Set( name , value ) }
	response , err := client.fixture.app.Test( request , fiber.TestConfig{ Timeout: 40 * time.Second } )
	if err != nil { t.Fatal( err ) }
	body , _ = io.ReadAll( response.Body )
	return
}

// libraryID is the library a user with no WebDAV name yet reads: their
// own user id (libraryOf).
func libraryID( fixture *authFixture ) ( id uint64 ) {
	id = fixture.alice.ID
	return
}

func TestLibrarySnapshot( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	put := func( kind string , key string , data fiber.Map ) {
		t.Helper()
		if status , reply := send( t , alice , "/api/library/" + kind + "/" + key , fiber.Map{ "version": 1 , "data": data } ); status != fiber.StatusOK { t.Fatalf( "%s %d %v" , key , status , reply ) }
	}
	put( "collections" , "COLL0001" , fiber.Map{ "name": "Reading" } )
	put( "items" , "PAPER001" , fiber.Map{ "itemType": "journalArticle" , "title": "A paper" } )
	put( "items" , "GONE0001" , fiber.Map{ "itemType": "book" , "title": "Deleted" } )
	send( t , alice , "/api/library/items/GONE0001/delete" , fiber.Map{} )
	library := libraryID( fixture )
	if err := models.NoteDAVProp( fixture.store , library , "FILE0001" , models.ZoteroProp( 1000 , "0123456789abcdef0123456789abcdef" ) ); err != nil { t.Fatal( err ) }

	response , body := alice.raw( t , "/api/library/snapshot" , map[string]string{ "Accept-Encoding": "gzip" } )
	if response.StatusCode != fiber.StatusOK || response.Header.Get( "Content-Encoding" ) != "gzip" { t.Fatalf( "%d %v" , response.StatusCode , response.Header ) }
	reader , err := gzip.NewReader( bytesReader( body ) )
	if err != nil { t.Fatal( err ) }
	plain , _ := io.ReadAll( reader )
	snapshot := librarySnapshot{}
	if err = json.Unmarshal( plain , &snapshot ); err != nil { t.Fatal( err ) }
	if len( snapshot.Items ) != 1 || snapshot.Items[ 0 ].Key != "PAPER001" || snapshot.Items[ 0 ].Data[ "title" ] != "A paper" { t.Fatalf( "items %+v" , snapshot.Items ) }
	if len( snapshot.Collections ) != 1 || len( snapshot.Attachments ) != 1 || snapshot.Attachments[ 0 ].MD5 == "" { t.Fatalf( "%+v" , snapshot ) }
	if snapshot.Rev != 4 { t.Fatalf( "rev %d" , snapshot.Rev ) }

	// Unchanged: 304. A change: a new ETag.
	tag := response.Header.Get( "ETag" )
	if again , _ := alice.raw( t , "/api/library/snapshot" , map[string]string{ "If-None-Match": tag } ); again.StatusCode != fiber.StatusNotModified { t.Fatalf( "%d" , again.StatusCode ) }
	models.NoteDAVZip( fixture.store , library , "FILE0001" , 10 )
	if again , _ := alice.raw( t , "/api/library/snapshot" , map[string]string{ "If-None-Match": tag } ); again.StatusCode != fiber.StatusOK { t.Fatalf( "files changed, still %d" , again.StatusCode ) }
	// Without gzip asked for, plain JSON.
	if plain , _ := alice.raw( t , "/api/library/snapshot" , nil ); plain.Header.Get( "Content-Encoding" ) != "" { t.Fatalf( "%v" , plain.Header ) }
}

func TestAttachmentsSince( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	library := libraryID( fixture )
	models.NoteDAVZip( fixture.store , library , "FILE0001" , 10 )
	_ , first := alice.do( t , http.MethodGet , "/api/library/attachments" , nil )
	rows := first[ "attachments" ].( []any )
	if len( rows ) != 1 { t.Fatalf( "%v" , first ) }
	newest := rows[ 0 ].( map[string]any )[ "updated_at" ].( string )
	time.Sleep( 2 * time.Millisecond )
	models.NoteDAVZip( fixture.store , library , "FILE0002" , 20 )
	_ , since := alice.do( t , http.MethodGet , "/api/library/attachments?since=" + newest , nil )
	rows = since[ "attachments" ].( []any )
	if len( rows ) != 1 || rows[ 0 ].( map[string]any )[ "key" ] != "FILE0002" { t.Fatalf( "%v" , since ) }
	if since[ "files_stamp" ].( float64 ) == first[ "files_stamp" ].( float64 ) { t.Fatalf( "files_stamp did not move" ) }
	if status , _ := alice.do( t , http.MethodGet , "/api/library/attachments?since=yesterday" , nil ); status != fiber.StatusBadRequest { t.Fatalf( "%d" , status ) }

	// The hourly rebuild of the index changes nothing that did not change.
	before := models.LibraryFilesStamp( library )
	rows_now , _ := models.ListDAVAttachments( fixture.store , library )
	copied := []*models.DAVAttachment{}
	for _ , row := range rows_now {
		again := *row
		again.UpdatedAt = time.Now().UTC()
		copied = append( copied , &again )
	}
	if err := models.ReplaceDAVAttachments( fixture.store , library , copied ); err != nil { t.Fatal( err ) }
	if models.LibraryFilesStamp( library ) != before { t.Fatalf( "a rebuild with nothing new moved files_stamp" ) }
	latest := time.Time{}
	for _ , row := range rows_now {
		if row.UpdatedAt.After( latest ) { latest = row.UpdatedAt }
	}
	_ , none := alice.do( t , http.MethodGet , "/api/library/attachments?since=" + latest.Format( time.RFC3339Nano ) , nil )
	if rows := none[ "attachments" ].( []any ); len( rows ) != 0 { t.Fatalf( "rebuild re-stamped rows: %v" , none ) }
}

func TestChangesWaits( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	library := libraryID( fixture )
	_ , now := alice.do( t , http.MethodGet , "/api/library/changes" , nil )
	rev := uint64( now[ "rev" ].( float64 ) )
	stamp := int64( now[ "files_stamp" ].( float64 ) )
	path := "/api/library/changes?wait=10&since=" + strconv.FormatUint( rev , 10 ) + "&files=" + strconv.FormatInt( stamp , 10 )

	// A record written while the request waits answers it at once.
	go func() {
		time.Sleep( 300 * time.Millisecond )
		models.UpsertLibrary( fixture.store , library , models.LibraryKindItem , []models.LibraryInput{ { Key: "PAPER001" , Version: 1 , Data: json.RawMessage( `{"itemType":"book","title":"New"}` ) } } )
	}()
	started := time.Now()
	_ , reply := alice.do( t , http.MethodGet , path , nil )
	if took := time.Since( started ); took > 5 * time.Second { t.Fatalf( "waited %v" , took ) }
	if records := reply[ "records" ].( []any ); len( records ) != 1 { t.Fatalf( "%v" , reply ) }

	// A file arriving answers it too, with a new files_stamp and no records.
	rev = uint64( reply[ "rev" ].( float64 ) )
	path = "/api/library/changes?wait=10&since=" + strconv.FormatUint( rev , 10 ) + "&files=" + strconv.FormatInt( stamp , 10 )
	go func() {
		time.Sleep( 300 * time.Millisecond )
		models.NoteDAVZip( fixture.store , library , "FILE0001" , 10 )
	}()
	started = time.Now()
	_ , reply = alice.do( t , http.MethodGet , path , nil )
	if took := time.Since( started ); took > 5 * time.Second { t.Fatalf( "waited %v" , took ) }
	if int64( reply[ "files_stamp" ].( float64 ) ) == stamp || len( reply[ "records" ].( []any ) ) != 0 { t.Fatalf( "%v" , reply ) }

	// Nothing changing: it answers when wait runs out.
	stamp = int64( reply[ "files_stamp" ].( float64 ) )
	started = time.Now()
	alice.do( t , http.MethodGet , "/api/library/changes?wait=1&since=" + strconv.FormatUint( rev , 10 ) + "&files=" + strconv.FormatInt( stamp , 10 ) , nil )
	if took := time.Since( started ); took < 900 * time.Millisecond || took > 5 * time.Second { t.Fatalf( "took %v" , took ) }
	if status , _ := alice.do( t , http.MethodGet , "/api/library/changes?wait=60" , nil ); status != fiber.StatusBadRequest { t.Fatalf( "wait=60: %d" , status ) }
}

func bytesReader( body []byte ) ( reader io.Reader ) {
	reader = &byteReader{ body: body }
	return
}

type byteReader struct{ body []byte }

func ( reader *byteReader ) Read( into []byte ) ( n int , err error ) {
	if len( reader.body ) == 0 {
		err = io.EOF
		return
	}
	n = copy( into , reader.body )
	reader.body = reader.body[ n: ]
	return
}

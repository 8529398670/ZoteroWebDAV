package routes

import (
	bytes "bytes"
	json "encoding/json"
	io "io"
	http "net/http"
	url "net/url"
	strconv "strconv"
	testing "testing"
	time "time"

	fiber "github.com/gofiber/fiber/v3"
)

// Moving a collection takes its subcollections and the items filed in it
// along; a dry run changes nothing.
func TestMoveCollections( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	put := func( kind string , key string , data fiber.Map ) {
		t.Helper()
		if status , reply := send( t , alice , "/api/library/" + kind + "/" + key , fiber.Map{ "version": 3 , "data": data } ); status != fiber.StatusOK { t.Fatalf( "%s %d %v" , key , status , reply ) }
	}
	put( "collections" , "MADEC001" , fiber.Map{ "name": "Top" , "parentCollection": false } )
	put( "collections" , "MADEC002" , fiber.Map{ "name": "Sub" , "parentCollection": "MADEC001" } )
	put( "collections" , "TAKENC01" , fiber.Map{ "name": "Taken" } )
	put( "items" , "ITEM0001" , fiber.Map{ "itemType": "book" , "title": "Filed" , "collections": []string{ "MADEC001" , "TAKENC01" } } )

	moves := []any{
		fiber.Map{ "from": "MADEC001" , "to": "REALC001" } ,
		fiber.Map{ "from": "MADEC002" , "to": "TAKENC01" } ,
	}
	status , reply := send( t , alice , "/api/library/collections/move" , fiber.Map{ "collections": moves , "dry_run": true } )
	if status != fiber.StatusOK { t.Fatalf( "dry run %d %v" , status , reply ) }
	_ , item := alice.do( t , http.MethodGet , "/api/library/items/ITEM0001" , nil )
	if data := item[ "data" ].( map[string]any ); data[ "collections" ].( []any )[ 0 ] != "MADEC001" { t.Fatalf( "a dry run wrote %v" , data ) }

	status , reply = send( t , alice , "/api/library/collections/move" , fiber.Map{ "collections": moves } )
	if status != fiber.StatusOK { t.Fatalf( "%d %v" , status , reply ) }
	results := reply[ "results" ].( []any )
	if row := results[ 0 ].( map[string]any ); row[ "status" ] != "moved" || row[ "children" ] != float64( 2 ) { t.Errorf( "move %v" , row ) }
	if row := results[ 1 ].( map[string]any ); row[ "status" ] != "refused" { t.Errorf( "taken %v" , row ) }
	_ , item = alice.do( t , http.MethodGet , "/api/library/items/ITEM0001" , nil )
	if listed := item[ "data" ].( map[string]any )[ "collections" ].( []any ); listed[ 0 ] != "REALC001" || listed[ 1 ] != "TAKENC01" { t.Errorf( "item %v" , listed ) }
	_ , collections := alice.do( t , http.MethodGet , "/api/library/collections" , nil )
	found := map[string]map[string]any{}
	for _ , raw := range collections[ "records" ].( []any ) {
		row := raw.( map[string]any )
		found[ row[ "key" ].( string ) ] = row
	}
	if found[ "MADEC001" ] != nil || found[ "REALC001" ] == nil { t.Errorf( "collections %v" , found ) }
	if sub := found[ "MADEC002" ]; sub == nil || sub[ "parent_key" ] != "REALC001" { t.Errorf( "sub %v" , sub ) }
}

func sendAttachmentPiece( t *testing.T , client *browser , query url.Values , body []byte ) ( status int , reply map[ string ]any ) {
	t.Helper()
	request , _ := http.NewRequest( http.MethodPost , AttachmentUploadPath + "?" + query.Encode() , bytes.NewReader( body ) )
	request.Header.Set( "Content-Type" , "application/octet-stream" )
	request.Header.Set( "X-CSRF-Token" , client.csrf( t ) )
	request.Header.Set( "Cookie" , client.cookie )
	response , err := client.fixture.app.Test( request , fiber.TestConfig{ Timeout: 10 * time.Second } )
	if err != nil { t.Fatal( err ) }
	text , _ := io.ReadAll( response.Body )
	status = response.StatusCode
	json.Unmarshal( text , &reply )
	return
}

// A linked file's attachment gets its file uploaded: stored as Zotero's sync
// stores it, and the item becomes an imported file.
func TestUploadAttachment( t *testing.T ) {
	fixture := libraryFixture( t )
	fixture.cfg.AppDir = t.TempDir()
	fixture.cfg.WebDAV.Dir = t.TempDir()
	fixture.cfg.WebDAV.MaxFileBytes = 64 * 1024 * 1024
	alice := fixture.signIn( t , fixture.alice.ID )
	if status , reply := send( t , alice , "/api/library/items/LINKED01" , fiber.Map{ "version": 3 , "data": fiber.Map{ "itemType": "attachment" , "linkMode": "linked_file" , "path": "/old/Café.pdf" , "title": "Café" } } ); status != fiber.StatusOK { t.Fatalf( "%d %v" , status , reply ) }

	whole := make( []byte , ImportChunkBytes + 10 )
	copy( whole , "%PDF-1.4" )
	query := func( offset int ) url.Values {
		return url.Values{ "key": { "LINKED01" } , "filename": { "Café.pdf" } , "size": { strconv.Itoa( len( whole ) ) } , "offset": { strconv.Itoa( offset ) } , "mtime": { "1600000000000" } }
	}
	status , reply := sendAttachmentPiece( t , alice , query( 0 ) , whole[ :ImportChunkBytes ] )
	if status != fiber.StatusOK || reply[ "done" ] != false { t.Fatalf( "first %d %v" , status , reply ) }
	status , reply = sendAttachmentPiece( t , alice , query( ImportChunkBytes ) , whole[ ImportChunkBytes: ] )
	if status != fiber.StatusOK || reply[ "done" ] != true { t.Fatalf( "last %d %v" , status , reply ) }

	_ , item := alice.do( t , http.MethodGet , "/api/library/items/LINKED01" , nil )
	data := item[ "data" ].( map[string]any )
	if data[ "linkMode" ] != "imported_file" || data[ "filename" ] != "Café.pdf" || data[ "path" ] != nil || data[ "contentType" ] != "application/pdf" { t.Errorf( "item %v" , data ) }
	if _ , got := alice.raw( t , "/api/library/attachments/LINKED01/file" , nil ); bytes.Equal( got , whole ) == false { t.Errorf( "file %d bytes back, want %d" , len( got ) , len( whole ) ) }

	// Not an attachment: refused.
	send( t , alice , "/api/library/items/BOOK0001" , fiber.Map{ "version": 3 , "data": fiber.Map{ "itemType": "book" , "title": "B" } } )
	other := url.Values{ "key": { "BOOK0001" } , "filename": { "a.pdf" } , "size": { "3" } , "offset": { "0" } }
	if status , _ = sendAttachmentPiece( t , alice , other , []byte( "abc" ) ); status != fiber.StatusNotFound { t.Errorf( "a book took a file: %d" , status ) }
	other.Set( "key" , "LINKED01" )
	other.Set( "filename" , "../x.pdf" )
	if status , _ = sendAttachmentPiece( t , alice , other , []byte( "abc" ) ); status != fiber.StatusBadRequest { t.Errorf( "a path took: %d" , status ) }
}

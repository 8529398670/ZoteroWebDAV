package routes

import (
	io "io"
	http "net/http"
	strings "strings"
	testing "testing"
	time "time"

	fiber "github.com/gofiber/fiber/v3"

	dav "zoterowebdav/server/dav"
)

// libraryFixture is the auth fixture with WebDAV switched on, which every
// library route needs.
func libraryFixture( t *testing.T ) ( fixture *authFixture ) {
	t.Helper()
	fixture = newAuthFixture( t )
	fixture.cfg.WebDAV.Enabled = true
	fixture.handlers.DAV = dav.New( fixture.store , fixture.handlers.Settings , nil )
	return
}

func TestDeletingAnItemWithItsChildren( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	put := func( key string , data fiber.Map ) {
		t.Helper()
		if status , reply := send( t , alice , "/api/library/items/" + key , fiber.Map{ "version": 3 , "data": data } ); status != fiber.StatusOK { t.Fatalf( "%s %d %v" , key , status , reply ) }
	}
	put( "PAPER001" , fiber.Map{ "itemType": "journalArticle" , "title": "A paper" } )
	put( "PDFFILE1" , fiber.Map{ "itemType": "attachment" , "parentItem": "PAPER001" , "filename": "a.pdf" } )
	put( "NOTE0001" , fiber.Map{ "itemType": "note" , "parentItem": "PAPER001" , "note": "hi" } )
	put( "OTHER001" , fiber.Map{ "itemType": "book" , "title": "Unrelated" } )

	status , reply := send( t , alice , "/api/library/items/PAPER001/delete?children=true" , fiber.Map{} )
	if status != fiber.StatusOK { t.Fatalf( "%d %v" , status , reply ) }
	removed , _ := reply[ "removed" ].( []any )
	if len( removed ) != 3 { t.Fatalf( "removed %v" , reply ) }
	first := removed[ 0 ].( map[string]any )
	if first[ "key" ] != "PAPER001" || first[ "data" ].( map[string]any )[ "title" ] != "A paper" { t.Fatalf( "first %v" , first ) }

	_ , list := alice.do( t , http.MethodGet , "/api/library/items" , nil )
	records , _ := list[ "records" ].( []any )
	if len( records ) != 1 || records[ 0 ].( map[string]any )[ "key" ] != "OTHER001" { t.Fatalf( "left %v" , list ) }

	// Without children only the item goes, and a second delete is a 404.
	put( "PAPER002" , fiber.Map{ "itemType": "book" , "title": "B" } )
	put( "PDFFILE2" , fiber.Map{ "itemType": "attachment" , "parentItem": "PAPER002" } )
	if status , reply = send( t , alice , "/api/library/items/PAPER002/delete" , fiber.Map{} ); status != fiber.StatusOK || reply[ "removed" ] != nil { t.Fatalf( "%d %v" , status , reply ) }
	if status , _ = send( t , alice , "/api/library/items/PAPER002/delete?children=true" , fiber.Map{} ); status != fiber.StatusNotFound { t.Fatalf( "%d" , status ) }
	if status , _ = alice.do( t , http.MethodGet , "/api/library/items/PDFFILE2" , nil ); status != fiber.StatusOK { t.Fatalf( "child went too: %d" , status ) }
}

func TestLibraryOpenPreference( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	if _ , me := alice.do( t , http.MethodGet , "/api/me" , nil ); me[ "library_open" ] != "tab" { t.Fatalf( "default %v" , me[ "library_open" ] ) }
	if status , reply := send( t , alice , "/api/account/library-open" , fiber.Map{ "mode": "download" } ); status != fiber.StatusOK { t.Fatalf( "%d %v" , status , reply ) }
	if _ , me := alice.do( t , http.MethodGet , "/api/me" , nil ); me[ "library_open" ] != "download" { t.Fatalf( "saved %v" , me[ "library_open" ] ) }
	if status , _ := send( t , alice , "/api/account/library-open" , fiber.Map{ "mode": "print" } ); status != fiber.StatusBadRequest { t.Fatalf( "%d" , status ) }
}

func TestLibrarySchema( t *testing.T ) {
	fixture := libraryFixture( t )
	alice := fixture.signIn( t , fixture.alice.ID )
	status , reply := alice.do( t , http.MethodGet , "/api/library/schema" , nil )
	if status != fiber.StatusOK { t.Fatalf( "%d %v" , status , reply ) }
	fields := reply[ "item_types" ].( map[string]any )[ "journalArticle" ].( []any )
	if fields[ 0 ] != "title" { t.Fatalf( "%v" , fields ) }
	if reply[ "creator_types" ].( map[string]any )[ "book" ].( []any )[ 0 ] != "author" { t.Fatalf( "%v" , reply[ "creator_types" ] ) }
	if reply[ "item_types" ].( map[string]any )[ "note" ] == nil { t.Fatal( "no note fields" ) }
}

func TestInlinePDFHeaders( t *testing.T ) {
	app := fiber.New()
	app.Get( "/" , func( c fiber.Ctx ) error {
		for _ , header := range inlinePDFDropped { c.Set( header , "set by the security middleware" ) }
		return sendInlinePDF( c , io.NopCloser( strings.NewReader( "%PDF-1.4" ) ) , 8 , "a paper.pdf" )
	} )
	request , _ := http.NewRequest( http.MethodGet , "/" , nil )
	response , err := app.Test( request , fiber.TestConfig{ Timeout: 5 * time.Second } )
	if err != nil { t.Fatal( err ) }
	if response.Header.Get( "Content-Type" ) != "application/pdf" { t.Fatal( response.Header ) }
	if disposition := response.Header.Get( "Content-Disposition" ); strings.HasPrefix( disposition , `inline; filename="a paper.pdf"` ) == false { t.Fatal( disposition ) }
	// Any CSP or X-Frame-Options stops Chrome's viewer drawing the page.
	for _ , header := range inlinePDFDropped {
		if response.Header.Get( header ) != "" { t.Fatalf( "%s: %s" , header , response.Header.Get( header ) ) }
	}
	if response.Header.Get( "X-Content-Type-Options" ) != "nosniff" { t.Fatal( "nosniff" ) }
}

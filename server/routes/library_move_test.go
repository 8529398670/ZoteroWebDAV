package routes

import (
	http "net/http"
	os "os"
	filepath "path/filepath"
	testing "testing"

	fiber "github.com/gofiber/fiber/v3"

	models "zoterowebdav/server/models"
)

// Moving an item to its real key takes its children and its attachment's
// files along, sets the fields given, and leaves tombstones; a move onto a
// taken key is refused while the rest go; a dry run changes nothing.
func TestMoveItems( t *testing.T ) {
	fixture := libraryFixture( t )
	fixture.cfg.WebDAV.Dir = t.TempDir()
	alice := fixture.signIn( t , fixture.alice.ID )
	library := fixture.alice.ID
	put := func( key string , data fiber.Map ) {
		t.Helper()
		if status , reply := send( t , alice , "/api/library/items/" + key , fiber.Map{ "version": 3 , "data": data } ); status != fiber.StatusOK { t.Fatalf( "%s %d %v" , key , status , reply ) }
	}
	put( "MADE0001" , fiber.Map{ "itemType": "journalArticle" , "title": "No DOI" } )
	put( "MADEPDF1" , fiber.Map{ "itemType": "attachment" , "parentItem": "MADE0001" , "linkMode": "imported_file" , "filename": "a.pdf" } )
	put( "MADENOT1" , fiber.Map{ "itemType": "note" , "parentItem": "MADE0001" , "note": "hi" } )
	put( "TAKEN001" , fiber.Map{ "itemType": "book" , "title": "Taken" } )
	put( "MADE0002" , fiber.Map{ "itemType": "book" , "title": "Clashes" } )

	folder := filepath.Join( fixture.handlers.DAV.Root( library ) , "zotero" )
	os.MkdirAll( folder , 0o700 )
	os.WriteFile( filepath.Join( folder , "MADEPDF1.zip" ) , []byte( "zip" ) , 0o600 )
	prop := models.ZoteroProp( 1600000000000 , "0123456789abcdef0123456789abcdef" )
	os.WriteFile( filepath.Join( folder , "MADEPDF1.prop" ) , prop , 0o600 )
	models.NoteDAVZip( fixture.store , library , "MADEPDF1" , 3 )
	models.NoteDAVProp( fixture.store , library , "MADEPDF1" , prop )

	moves := []any{
		fiber.Map{ "from": "MADE0001" , "to": "REAL0001" , "set": fiber.Map{ "dateAdded": "2020-01-02T03:04:05Z" } } ,
		fiber.Map{ "from": "MADEPDF1" , "to": "REALPDF1" } ,
		fiber.Map{ "from": "MADE0002" , "to": "TAKEN001" } ,
		fiber.Map{ "from": "TAKEN001" , "set": fiber.Map{ "dateAdded": "2019-01-01T00:00:00Z" } } ,
	}
	status , reply := send( t , alice , "/api/library/items/move" , fiber.Map{ "items": moves , "dry_run": true } )
	if status != fiber.StatusOK { t.Fatalf( "dry run %d %v" , status , reply ) }
	if status , _ = alice.do( t , http.MethodGet , "/api/library/items/REAL0001" , nil ); status != fiber.StatusNotFound { t.Fatalf( "a dry run wrote: %d" , status ) }
	if _ , err := os.Stat( filepath.Join( folder , "MADEPDF1.zip" ) ); err != nil { t.Fatal( "a dry run moved files" ) }

	status , reply = send( t , alice , "/api/library/items/move" , fiber.Map{ "items": moves } )
	if status != fiber.StatusOK { t.Fatalf( "%d %v" , status , reply ) }
	results := reply[ "results" ].( []any )
	want := []string{ "moved" , "moved" , "refused" , "set" }
	for index , status := range want {
		row := results[ index ].( map[string]any )
		if row[ "status" ] != status { t.Errorf( "move %d: %v" , index , row ) }
	}
	if row := results[ 0 ].( map[string]any ); row[ "children" ] != float64( 2 ) { t.Errorf( "children %v" , row ) }
	if row := results[ 1 ].( map[string]any ); row[ "files" ] != true { t.Errorf( "files %v" , row ) }

	_ , item := alice.do( t , http.MethodGet , "/api/library/items/REAL0001" , nil )
	if data := item[ "data" ].( map[string]any ); data[ "dateAdded" ] != "2020-01-02T03:04:05Z" || data[ "key" ] != "REAL0001" { t.Errorf( "moved %v" , data ) }
	_ , child := alice.do( t , http.MethodGet , "/api/library/items/REALPDF1" , nil )
	if data := child[ "data" ].( map[string]any ); data[ "parentItem" ] != "REAL0001" { t.Errorf( "attachment %v" , data ) }
	_ , note := alice.do( t , http.MethodGet , "/api/library/items/MADENOT1" , nil )
	if data := note[ "data" ].( map[string]any ); data[ "parentItem" ] != "REAL0001" { t.Errorf( "note %v" , data ) }
	if status , _ = alice.do( t , http.MethodGet , "/api/library/items/MADE0001" , nil ); status == fiber.StatusOK {
		if stored , _ := models.GetLibraryRecord( fixture.store , library , models.LibraryKindItem , "MADE0001" ); stored.Deleted == false { t.Error( "no tombstone" ) }
	}
	if _ , err := os.Stat( filepath.Join( folder , "REALPDF1.zip" ) ); err != nil { t.Error( "the zip did not move" ) }
	if _ , err := os.Stat( filepath.Join( folder , "MADEPDF1.prop" ) ); err == nil { t.Error( "the old prop is still there" ) }
	if row , err := models.GetDAVAttachment( fixture.store , library , "REALPDF1" ); err != nil || row.MD5 != "0123456789abcdef0123456789abcdef" { t.Errorf( "index %v %v" , row , err ) }
	_ , taken := alice.do( t , http.MethodGet , "/api/library/items/TAKEN001" , nil )
	if data := taken[ "data" ].( map[string]any ); data[ "title" ] != "Taken" || data[ "dateAdded" ] != "2019-01-01T00:00:00Z" { t.Errorf( "taken %v" , data ) }
}

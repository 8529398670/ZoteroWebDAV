package routes

import (
	bytes "bytes"
	json "encoding/json"
	io "io"
	http "net/http"
	os "os"
	strconv "strconv"
	testing "testing"
	time "time"

	fiber "github.com/gofiber/fiber/v3"
)

// sendPiece posts one upload piece as raw bytes, the way the page does.
func sendPiece( t *testing.T , client *browser , offset int64 , body []byte ) ( status int , reply map[ string ]any ) {
	t.Helper()
	request , _ := http.NewRequest( http.MethodPost , ImportChunkPath + "?offset=" + strconv.FormatInt( offset , 10 ) , bytes.NewReader( body ) )
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

// The page sends a few pieces at once, so they arrive in any order; each
// lands at its offset, a piece sent twice counts once, and the import may
// start only once every byte is in.
func TestUploadPiecesInAnyOrder( t *testing.T ) {
	fixture := libraryFixture( t )
	fixture.cfg.AppDir = t.TempDir()
	fixture.cfg.WebDAV.ImportMaxBytes = 64 * 1024 * 1024
	alice := fixture.signIn( t , fixture.alice.ID )

	whole := make( []byte , 2 * ImportChunkBytes + 10 )
	for i := range whole { whole[ i ] = byte( i * 7 ) }
	size := int64( len( whole ) )
	piece := func( index int64 ) ( offset int64 , body []byte ) {
		offset = index * ImportChunkBytes
		end := min( offset + ImportChunkBytes , size )
		body = whole[ offset : end ]
		return
	}

	status , reply := send( t , alice , "/api/library/import/begin" , fiber.Map{ "format": "rdf" , "name": "export.rdf" , "size": size } )
	if status != fiber.StatusOK { t.Fatalf( "begin %d %v" , status , reply ) }

	// Last first, then the first, then the first again.
	for _ , index := range []int64{ 2 , 0 , 0 } {
		offset , body := piece( index )
		if status , reply = sendPiece( t , alice , offset , body ); status != fiber.StatusOK { t.Fatalf( "piece %d: %d %v" , index , status , reply ) }
	}
	if reply[ "received" ] != float64( ImportChunkBytes + 10 ) { t.Fatalf( "received %v" , reply ) }

	// Not done yet, so it can't start.
	if status , _ = send( t , alice , "/api/library/import/finish" , fiber.Map{} ); status != fiber.StatusConflict { t.Fatalf( "finish early %d" , status ) }

	// A piece off the grid, or a short one that isn't the last, is refused.
	if status , _ = sendPiece( t , alice , 5 , whole[ 5 : 5 + ImportChunkBytes ] ); status != fiber.StatusBadRequest { t.Fatalf( "unaligned %d" , status ) }
	if status , _ = sendPiece( t , alice , ImportChunkBytes , whole[ ImportChunkBytes : ImportChunkBytes + 10 ] ); status != fiber.StatusBadRequest { t.Fatalf( "short %d" , status ) }

	offset , body := piece( 1 )
	if status , reply = sendPiece( t , alice , offset , body ); status != fiber.StatusOK || reply[ "received" ] != float64( size ) { t.Fatalf( "middle %d %v" , status , reply ) }

	state := importFor( fixture.alice.ID )
	state.mu.Lock()
	path := state.upload.path
	state.mu.Unlock()
	stored , err := os.ReadFile( path )
	if err != nil { t.Fatal( err ) }
	if bytes.Equal( stored , whole ) == false { t.Fatalf( "stored %d bytes, not the file" , len( stored ) ) }
}

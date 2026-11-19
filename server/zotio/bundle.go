package zotio

import (
	zip "archive/zip"
	bytes "bytes"
	errors "errors"
	io "io"
	mime "mime"
	url "net/url"
	os "os"
	path "path"
	filepath "path/filepath"
	strings "strings"
)

// An upload is either one export file, or a zip of the folder Zotero writes
// when "Export Files" is ticked: the export file, and files/<n>/<name> beside
// it. A Bundle finds the files the export names.

// ErrNoExport is a zip with no file of the format asked for in it.
var ErrNoExport = errors.New( "the zip has no export file of that format in it" )

// Bundle resolves a path an export names to a file.
type Bundle interface {
	Find( name string ) ( file File , found bool )
}

// Upload is an opened upload: the export itself, and its files if it came
// in a zip.
type Upload struct {
	Main   io.ReadCloser
	Bundle Bundle
	closer io.Closer
}

func ( upload *Upload ) Close() ( err error ) {
	upload.Main.Close()
	if upload.closer != nil { err = upload.closer.Close() }
	return
}

// OpenUpload opens what was uploaded to path, as format.
func OpenUpload( full string , format Format ) ( upload *Upload , err error ) {
	head := make( []byte , 4 )
	file , err := os.Open( full )
	if err != nil { return }
	io.ReadFull( file , head )
	if bytes.Equal( head , []byte( "PK\x03\x04" ) ) == false {
		if _ , err = file.Seek( 0 , io.SeekStart ); err != nil {
			file.Close()
			return
		}
		upload = &Upload{ Main: file }
		return
	}
	file.Close()
	reader , err := zip.OpenReader( full )
	if err != nil { return }
	var main *zip.File
	for _ , entry := range reader.File {
		if strings.HasSuffix( entry.Name , "/" ) || strings.Contains( entry.Name , "__MACOSX" ) { continue }
		if strings.EqualFold( path.Ext( entry.Name ) , "." + format.Ext ) == false { continue }
		// The shallowest one: an export inside files/ is an attachment.
		if main == nil || strings.Count( entry.Name , "/" ) < strings.Count( main.Name , "/" ) { main = entry }
	}
	if main == nil {
		reader.Close()
		err = ErrNoExport
		return
	}
	opened , err := main.Open()
	if err != nil {
		reader.Close()
		return
	}
	bundle := &zipBundle{ base: path.Dir( main.Name ) , entries: map[string]*zip.File{} }
	for _ , entry := range reader.File {
		if strings.HasSuffix( entry.Name , "/" ) == false { bundle.entries[ entry.Name ] = entry }
	}
	upload = &Upload{ Main: opened , Bundle: bundle , closer: reader }
	return
}

type zipBundle struct {
	base    string
	entries map[string]*zip.File
}

func ( bundle *zipBundle ) Find( name string ) ( file File , found bool ) {
	candidates := []string{ name }
	if decoded , err := url.PathUnescape( name ); err == nil && decoded != name { candidates = append( candidates , decoded ) }
	for _ , candidate := range candidates {
		candidate = strings.TrimPrefix( filepath.ToSlash( candidate ) , "file://" )
		wanted := path.Clean( path.Join( bundle.base , candidate ) )
		if strings.HasPrefix( candidate , "/" ) || strings.Contains( candidate , ":" ) {
			// An absolute path from the machine that exported: match its
			// last two parts (the attachment's folder and the file).
			parts := strings.Split( strings.ReplaceAll( candidate , "\\" , "/" ) , "/" )
			if len( parts ) < 2 { continue }
			tail := "/" + strings.Join( parts[ len( parts ) - 2: ] , "/" )
			for entry_name , entry := range bundle.entries {
				if strings.HasSuffix( "/" + entry_name , tail ) {
					file , found = zipFile( entry ) , true
					return
				}
			}
			continue
		}
		if entry := bundle.entries[ strings.TrimPrefix( wanted , "./" ) ]; entry != nil {
			file , found = zipFile( entry ) , true
			return
		}
	}
	return
}

func zipFile( entry *zip.File ) ( file File ) {
	file = File{
		Name:    path.Base( entry.Name ),
		Size:    int64( entry.UncompressedSize64 ),
		ModTime: entry.Modified,
		Open:    func() ( io.ReadCloser , error ) { return entry.Open() },
	}
	return
}

// contentTypeOf guesses a file's type from its name.
func contentTypeOf( name string ) ( content_type string ) {
	content_type = mime.TypeByExtension( strings.ToLower( path.Ext( name ) ) )
	if semi := strings.Index( content_type , ";" ); semi >= 0 { content_type = content_type[ :semi ] }
	if content_type == "" { content_type = "application/octet-stream" }
	return
}

// attach makes an attachment item under parent (or standalone, with parent
// nil) for a path an export names: an imported file when the bundle has it,
// a link when it is a URL, and a linked file, with a warning, when it is
// neither.
func ( library *Library ) attach( keys *keyMaker , bundle Bundle , parent Item , location string , title string , content_type string , key string ) ( attachment Item ) {
	location = strings.TrimSpace( location )
	parent_key := ""
	if parent != nil { parent_key = Str( parent , "key" ) }
	if key == "" { key = keys.make( parent_key + "|attachment|" + location + "|" + title ) }
	attachment = Item{ "itemType": "attachment" , "key": key }
	if parent_key != "" { attachment[ "parentItem" ] = parent_key }
	lower := strings.ToLower( location )
	var file File
	found := false
	if bundle != nil && location != "" { file , found = bundle.Find( location ) }
	switch {
	case found:
		attachment[ "linkMode" ] = "imported_file"
		attachment[ "filename" ] = file.Name
		if content_type == "" { content_type = contentTypeOf( file.Name ) }
		attachment[ "contentType" ] = content_type
		if title == "" { title = file.Name }
		library.Files[ Str( attachment , "key" ) ] = []File{ file }
	case strings.HasPrefix( lower , "http://" ) || strings.HasPrefix( lower , "https://" ):
		attachment[ "linkMode" ] = "linked_url"
		attachment[ "url" ] = location
		if title == "" { title = location }
	default:
		attachment[ "linkMode" ] = "linked_file"
		attachment[ "path" ] = location
		if content_type == "" && location != "" { content_type = contentTypeOf( location ) }
		if content_type != "" { attachment[ "contentType" ] = content_type }
		if title == "" { title = path.Base( filepath.ToSlash( location ) ) }
		if location != "" { library.warn( "file not in the upload: %s" , location ) }
	}
	attachment[ "title" ] = title
	library.Items = append( library.Items , attachment )
	return
}

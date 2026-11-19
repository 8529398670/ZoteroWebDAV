package zotio

import (
	sql "database/sql"
	errors "errors"
	fmt "fmt"
	io "io"
	os "os"
	filepath "path/filepath"
	strings "strings"

	_ "modernc.org/sqlite"
)

// A Zotero data folder read in place: zotero.sqlite for the items and
// collections of the personal library, with their real keys, and
// storage/<KEY>/ for each stored attachment's files. Nothing in the folder is
// written to: the database is copied first (Zotero keeps it locked while it
// runs) and the copy is read.

// ErrNoDatabase is a folder with no zotero.sqlite in it or above it.
var ErrNoDatabase = errors.New( "no zotero.sqlite there: give the Zotero data folder (Zotero > Settings > Advanced > Files and Folders shows it)" )

// FindDataFolder is the data folder for what someone typed: the folder
// itself, its storage/ folder, or zotero.sqlite.
func FindDataFolder( given string ) ( folder string , err error ) {
	given = strings.TrimSpace( given )
	if strings.HasPrefix( given , "~" ) {
		if home , home_err := os.UserHomeDir(); home_err == nil { given = filepath.Join( home , given[ 1: ] ) }
	}
	given = filepath.Clean( given )
	for _ , candidate := range []string{ given , filepath.Dir( given ) } {
		if info , stat_err := os.Stat( filepath.Join( candidate , "zotero.sqlite" ) ); stat_err == nil && info.Mode().IsRegular() {
			folder = candidate
			return
		}
	}
	err = ErrNoDatabase
	return
}

var linkModes = map[int64]string{ 0: "imported_file" , 1: "imported_url" , 2: "linked_file" , 3: "linked_url" , 4: "embedded_image" }

// ReadDataFolder reads a Zotero data folder's personal library. temp_dir is
// where the database copy goes. with_files lists each stored attachment's
// files from storage/.
func ReadDataFolder( folder string , temp_dir string , with_files bool ) ( library *Library , err error ) {
	copy_dir , err := os.MkdirTemp( temp_dir , "zotero-db-" )
	if err != nil { return }
	defer os.RemoveAll( copy_dir )
	// A journal or WAL beside it carries changes in progress; copying them
	// too gives SQLite what it needs to read a consistent database.
	for _ , suffix := range []string{ "" , "-journal" , "-wal" } {
		source := filepath.Join( folder , "zotero.sqlite" + suffix )
		if _ , stat_err := os.Stat( source ); stat_err != nil {
			if suffix == "" {
				err = ErrNoDatabase
				return
			}
			continue
		}
		if err = copyFile( source , filepath.Join( copy_dir , "zotero.sqlite" + suffix ) ); err != nil { return }
	}
	var files func( key string ) []File
	if with_files { files = func( key string ) []File { return folderStorage( filepath.Join( folder , "storage" , key ) ) } }
	library , err = readDatabase( copy_dir , files )
	return
}

// readDatabase reads copy_dir/zotero.sqlite. files lists an attachment's
// stored files by its key; nil leaves files out.
func readDatabase( copy_dir string , files func( key string ) []File ) ( library *Library , err error ) {
	db , err := sql.Open( "sqlite" , "file:" + filepath.Join( copy_dir , "zotero.sqlite" ) + "?_pragma=busy_timeout(5000)" )
	if err != nil { return }
	defer db.Close()
	reader := &sqliteReader{ db: db , library: NewLibrary() , files: files }
	// Zotero's own keys: the import keeps them rather than matching records
	// to ones already stored.
	reader.library.OwnKeys = true
	err = reader.read()
	if err != nil {
		err = fmt.Errorf( "reading zotero.sqlite: %w" , err )
		return
	}
	library = reader.library
	return
}

func copyFile( source string , target string ) ( err error ) {
	in , err := os.Open( source )
	if err != nil { return }
	defer in.Close()
	out , err := os.OpenFile( target , os.O_WRONLY | os.O_CREATE | os.O_EXCL , 0o600 )
	if err != nil { return }
	if _ , err = io.Copy( out , in ); err != nil {
		out.Close()
		return
	}
	err = out.Close()
	return
}

type sqliteReader struct {
	db      *sql.DB
	library *Library
	files   func( key string ) []File
	user    int64
	items   map[int64]Item
	keys    map[int64]string
}

// each runs query and calls row for every row.
func ( reader *sqliteReader ) each( query string , row func( scan func( ...any ) error ) error , args ...any ) ( err error ) {
	rows , err := reader.db.Query( query , args... )
	if err != nil { return }
	defer rows.Close()
	for rows.Next() {
		if err = row( rows.Scan ); err != nil { return }
	}
	err = rows.Err()
	return
}

// zoteroTime is "2020-01-02 03:04:05" as the web API writes it.
func zoteroTime( value string ) ( result string ) {
	result = csvTime( value )
	return
}

func ( reader *sqliteReader ) read() ( err error ) {
	if err = reader.db.QueryRow( `SELECT libraryID FROM libraries WHERE type = 'user' ORDER BY libraryID LIMIT 1` ).Scan( &reader.user ); err != nil { return }
	if err = reader.collections(); err != nil { return }
	reader.items = map[int64]Item{}
	reader.keys = map[int64]string{}
	order := []int64{}
	err = reader.each( `
		SELECT i.itemID , i.key , i.version , t.typeName , i.dateAdded , i.dateModified
		FROM items i JOIN itemTypesCombined t ON t.itemTypeID = i.itemTypeID
		WHERE i.libraryID = ? AND i.itemID NOT IN ( SELECT itemID FROM deletedItems ) AND t.typeName != 'annotation'
		ORDER BY i.itemID` , func( scan func( ...any ) error ) ( row_err error ) {
		var id , version int64
		var key , item_type , added , modified string
		if row_err = scan( &id , &key , &version , &item_type , &added , &modified ); row_err != nil { return }
		reader.items[ id ] = Item{
			"key": key , "version": version , "itemType": item_type ,
			"dateAdded": zoteroTime( added ) , "dateModified": zoteroTime( modified ) ,
		}
		reader.keys[ id ] = key
		order = append( order , id )
		return
	} , reader.user )
	if err != nil { return }
	steps := []func() error{ reader.fields , reader.creators , reader.tags , reader.membership , reader.notes , reader.relations }
	for _ , step := range steps {
		if err = step(); err != nil { return }
	}
	if err = reader.attachments(); err != nil { return }
	for _ , id := range order {
		item := reader.items[ id ]
		if item == nil { continue }
		if item[ "itemType" ] != "note" && item[ "itemType" ] != "attachment" {
			if _ , has := item[ "creators" ]; has == false { item[ "creators" ] = []any{} }
		}
		if _ , has := item[ "tags" ]; has == false { item[ "tags" ] = []any{} }
		if _ , has := item[ "relations" ]; has == false { item[ "relations" ] = map[string]any{} }
		if Str( item , "parentItem" ) == "" {
			if _ , has := item[ "collections" ]; has == false { item[ "collections" ] = []any{} }
		}
		reader.library.Items = append( reader.library.Items , item )
	}
	return
}

func ( reader *sqliteReader ) collections() ( err error ) {
	type row struct {
		key , name string
		version    int64
		parent     sql.NullInt64
	}
	rows := map[int64]row{}
	order := []int64{}
	err = reader.each( `SELECT collectionID , key , collectionName , version , parentCollectionID FROM collections WHERE libraryID = ?
		AND collectionID NOT IN ( SELECT collectionID FROM deletedCollections ) ORDER BY collectionID` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		found := row{}
		if row_err = scan( &id , &found.key , &found.name , &found.version , &found.parent ); row_err != nil { return }
		rows[ id ] = found
		order = append( order , id )
		return
	} , reader.user )
	if err != nil { return }
	for _ , id := range order {
		found := rows[ id ]
		collection := Item{ "key": found.key , "version": found.version , "name": found.name , "parentCollection": false , "relations": map[string]any{} }
		if found.parent.Valid {
			if parent , ok := rows[ found.parent.Int64 ]; ok { collection[ "parentCollection" ] = parent.key }
		}
		reader.library.Collections = append( reader.library.Collections , collection )
	}
	return
}

func ( reader *sqliteReader ) fields() ( err error ) {
	err = reader.each( `
		SELECT d.itemID , f.fieldName , v.value FROM itemData d
		JOIN items i ON i.itemID = d.itemID JOIN fieldsCombined f ON f.fieldID = d.fieldID JOIN itemDataValues v ON v.valueID = d.valueID
		WHERE i.libraryID = ?` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		var field string
		var value any
		if row_err = scan( &id , &field , &value ); row_err != nil { return }
		if item := reader.items[ id ]; item != nil {
			switch typed := value.( type ) {
			case []byte:
				item[ field ] = string( typed )
			case nil:
			default:
				item[ field ] = fmt.Sprint( typed )
			}
		}
		return
	} , reader.user )
	return
}

func ( reader *sqliteReader ) creators() ( err error ) {
	err = reader.each( `
		SELECT ic.itemID , c.firstName , c.lastName , c.fieldMode , t.creatorType FROM itemCreators ic
		JOIN items i ON i.itemID = ic.itemID JOIN creators c ON c.creatorID = ic.creatorID JOIN creatorTypes t ON t.creatorTypeID = ic.creatorTypeID
		WHERE i.libraryID = ? ORDER BY ic.itemID , ic.orderIndex` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		var first , last sql.NullString
		var mode sql.NullInt64
		var creator_type string
		if row_err = scan( &id , &first , &last , &mode , &creator_type ); row_err != nil { return }
		item := reader.items[ id ]
		if item == nil { return }
		creator := map[string]any{ "creatorType": creator_type }
		if mode.Int64 == 1 {
			creator[ "name" ] = last.String
		} else {
			creator[ "firstName" ] , creator[ "lastName" ] = first.String , last.String
		}
		list , _ := item[ "creators" ].( []any )
		item[ "creators" ] = append( list , creator )
		return
	} , reader.user )
	return
}

func ( reader *sqliteReader ) tags() ( err error ) {
	err = reader.each( `
		SELECT it.itemID , t.name , it.type FROM itemTags it JOIN items i ON i.itemID = it.itemID JOIN tags t ON t.tagID = it.tagID
		WHERE i.libraryID = ? ORDER BY it.itemID , t.name` , func( scan func( ...any ) error ) ( row_err error ) {
		var id , tag_type int64
		var name string
		if row_err = scan( &id , &name , &tag_type ); row_err != nil { return }
		if item := reader.items[ id ]; item != nil {
			entry := map[string]any{ "tag": name }
			if tag_type != 0 { entry[ "type" ] = tag_type }
			list , _ := item[ "tags" ].( []any )
			item[ "tags" ] = append( list , entry )
		}
		return
	} , reader.user )
	return
}

func ( reader *sqliteReader ) membership() ( err error ) {
	err = reader.each( `
		SELECT ci.itemID , c.key FROM collectionItems ci JOIN collections c ON c.collectionID = ci.collectionID
		WHERE c.libraryID = ? ORDER BY ci.itemID , ci.orderIndex` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		var key string
		if row_err = scan( &id , &key ); row_err != nil { return }
		if item := reader.items[ id ]; item != nil { addToCollection( item , key ) }
		return
	} , reader.user )
	return
}

func ( reader *sqliteReader ) notes() ( err error ) {
	err = reader.each( `
		SELECT n.itemID , n.parentItemID , n.note FROM itemNotes n JOIN items i ON i.itemID = n.itemID
		WHERE i.libraryID = ?` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		var parent sql.NullInt64
		var note sql.NullString
		if row_err = scan( &id , &parent , &note ); row_err != nil { return }
		item := reader.items[ id ]
		if item == nil { return }
		item[ "note" ] = note.String
		if parent.Valid {
			if key := reader.keys[ parent.Int64 ]; key != "" && reader.items[ parent.Int64 ] != nil { item[ "parentItem" ] = key }
		}
		return
	} , reader.user )
	return
}

func ( reader *sqliteReader ) relations() ( err error ) {
	err = reader.each( `
		SELECT r.itemID , p.predicate , r.object FROM itemRelations r JOIN items i ON i.itemID = r.itemID
		JOIN relationPredicates p ON p.predicateID = r.predicateID WHERE i.libraryID = ?` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		var predicate , object string
		if row_err = scan( &id , &predicate , &object ); row_err != nil { return }
		item := reader.items[ id ]
		if item == nil { return }
		relations , _ := item[ "relations" ].( map[string]any )
		if relations == nil { relations = map[string]any{} }
		list , _ := relations[ predicate ].( []any )
		relations[ predicate ] = append( list , object )
		item[ "relations" ] = relations
		return
	} , reader.user )
	return
}

func ( reader *sqliteReader ) attachments() ( err error ) {
	err = reader.each( `
		SELECT a.itemID , a.parentItemID , a.linkMode , a.contentType , cs.charset , a.path , a.storageModTime , a.storageHash
		FROM itemAttachments a JOIN items i ON i.itemID = a.itemID LEFT JOIN charsets cs ON cs.charsetID = a.charsetID
		WHERE i.libraryID = ?` , func( scan func( ...any ) error ) ( row_err error ) {
		var id int64
		var parent , mode , mtime sql.NullInt64
		var content_type , charset , location , hash sql.NullString
		if row_err = scan( &id , &parent , &mode , &content_type , &charset , &location , &mtime , &hash ); row_err != nil { return }
		item := reader.items[ id ]
		if item == nil { return }
		link_mode := linkModes[ mode.Int64 ]
		if link_mode == "embedded_image" {
			// An image inside a note or an annotation: part of its parent.
			delete( reader.items , id )
			return
		}
		item[ "linkMode" ] = link_mode
		item[ "contentType" ] = content_type.String
		item[ "charset" ] = charset.String
		if parent.Valid {
			if key := reader.keys[ parent.Int64 ]; key != "" && reader.items[ parent.Int64 ] != nil { item[ "parentItem" ] = key }
		}
		switch link_mode {
		case "imported_file" , "imported_url":
			name := strings.TrimPrefix( location.String , "storage:" )
			if name != "" { item[ "filename" ] = name }
			if hash.Valid && hash.String != "" { item[ "md5" ] = hash.String }
			if mtime.Valid && mtime.Int64 > 0 { item[ "mtime" ] = mtime.Int64 }
			if reader.files != nil {
				if files := reader.files( Str( item , "key" ) ); len( files ) > 0 {
					reader.library.Files[ Str( item , "key" ) ] = files
				} else {
					reader.library.warn( "no file in storage/%s" , Str( item , "key" ) )
				}
			}
		case "linked_file":
			item[ "path" ] = location.String
		}
		return
	} , reader.user )
	return
}

// folderStorage lists an attachment's folder, storage/<KEY>/: what Zotero's
// WebDAV sync zips, its own .zotero files left out.
func folderStorage( root string ) ( files []File ) {
	filepath.WalkDir( root , func( full string , entry os.DirEntry , walk_err error ) error {
		if walk_err != nil { return nil }
		name := entry.Name()
		if entry.IsDir() {
			if full != root && strings.HasPrefix( name , "." ) { return filepath.SkipDir }
			return nil
		}
		if strings.HasPrefix( name , "." ) || entry.Type().IsRegular() == false { return nil }
		info , info_err := entry.Info()
		if info_err != nil { return nil }
		relative , _ := filepath.Rel( root , full )
		opened := full
		files = append( files , File{
			Name: filepath.ToSlash( relative ) , Size: info.Size() , ModTime: info.ModTime() ,
			Open: func() ( io.ReadCloser , error ) { return os.Open( opened ) } ,
		} )
		return nil
	} )
	return
}

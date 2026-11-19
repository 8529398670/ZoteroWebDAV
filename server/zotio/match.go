package zotio

import (
	strings "strings"
	unicode "unicode"
)

// Match points records read from a file with no keys (RIS, an RDF from
// elsewhere, CSV's notes and attachments) at the records already in the
// library they stand for, so a merge updates instead of adding a second
// copy. A record whose key is already in the library is left alone. Top
// items match by DOI, else by type, title and year; notes by their text;
// attachments by file name, else title or URL; collections by their path of
// names. A match ignores the item type, which some formats cannot carry
// (RIS brings a preprint back as a journal article). Children only match under the parent their item matched.
func Match( library *Library , items []Item , collections []Item ) {
	existing := map[string]bool{}
	for _ , item := range items { existing[ Str( item , "key" ) ] = true }
	for _ , collection := range collections { existing[ Str( collection , "key" ) ] = true }
	renamed := map[string]string{}
	taken := map[string]bool{}

	// Collections, by path.
	paths := collectionPaths( collections )
	by_path := map[string]string{}
	for key , path := range paths { by_path[ path ] = key }
	imported_paths := collectionPaths( library.Collections )
	for _ , collection := range library.Collections {
		key := Str( collection , "key" )
		if existing[ key ] { continue }
		if found := by_path[ imported_paths[ key ] ]; found != "" && taken[ found ] == false {
			renamed[ key ] , taken[ found ] = found , true
		}
	}

	// Top items.
	// Lists: a library can hold the same paper twice.
	by_doi , by_title := map[string][]string{} , map[string][]string{}
	children := map[string][]Item{}
	for _ , item := range items {
		if parent := Str( item , "parentItem" ); parent != "" {
			children[ parent ] = append( children[ parent ] , item )
			continue
		}
		if doi := normalDOI( Str( item , "DOI" ) ); doi != "" { by_doi[ doi ] = append( by_doi[ doi ] , Str( item , "key" ) ) }
		by_title[ titleKey( item ) ] = append( by_title[ titleKey( item ) ] , Str( item , "key" ) )
	}
	for _ , item := range library.Items {
		key := Str( item , "key" )
		if existing[ key ] || Str( item , "parentItem" ) != "" { continue }
		free := func( keys []string ) ( first string ) {
			for _ , candidate := range keys {
				if taken[ candidate ] == false { return candidate }
			}
			return
		}
		found := ""
		if doi := normalDOI( Str( item , "DOI" ) ); doi != "" { found = free( by_doi[ doi ] ) }
		if found == "" && normalTitle( Str( item , "title" ) ) != "" { found = free( by_title[ titleKey( item ) ] ) }
		if found != "" { renamed[ key ] , taken[ found ] = found , true }
	}

	// Children, under the parent each item now has.
	for _ , item := range library.Items {
		key := Str( item , "key" )
		parent := Str( item , "parentItem" )
		if existing[ key ] || parent == "" { continue }
		if mapped := renamed[ parent ]; mapped != "" { parent = mapped }
		for _ , candidate := range children[ parent ] {
			other := Str( candidate , "key" )
			if taken[ other ] || Str( candidate , "itemType" ) != Str( item , "itemType" ) || sameChild( item , candidate ) == false { continue }
			renamed[ key ] , taken[ other ] = other , true
			break
		}
	}
	if len( renamed ) == 0 { return }

	// Put the new keys everywhere the old ones were.
	for _ , collection := range library.Collections {
		if mapped := renamed[ Str( collection , "key" ) ]; mapped != "" { collection[ "key" ] = mapped }
		if mapped := renamed[ Str( collection , "parentCollection" ) ]; mapped != "" { collection[ "parentCollection" ] = mapped }
	}
	for _ , item := range library.Items {
		old := Str( item , "key" )
		if mapped := renamed[ old ]; mapped != "" {
			item[ "key" ] = mapped
			if files , ok := library.Files[ old ]; ok {
				delete( library.Files , old )
				library.Files[ mapped ] = files
			}
		}
		if mapped := renamed[ Str( item , "parentItem" ) ]; mapped != "" { item[ "parentItem" ] = mapped }
		if list , ok := item[ "collections" ].( []any ); ok {
			for index , each := range list {
				if text , ok := each.( string ); ok && renamed[ text ] != "" { list[ index ] = renamed[ text ] }
			}
		}
	}
}

func collectionPaths( collections []Item ) ( paths map[string]string ) {
	names , parents := map[string]string{} , map[string]string{}
	for _ , collection := range collections {
		key := Str( collection , "key" )
		names[ key ] , parents[ key ] = Str( collection , "name" ) , Str( collection , "parentCollection" )
	}
	paths = map[string]string{}
	for key := range names {
		path := names[ key ]
		for up , depth := parents[ key ] , 0; up != "" && names[ up ] != "" && depth < 50; up , depth = parents[ up ] , depth + 1 {
			path = names[ up ] + "/" + path
		}
		paths[ key ] = strings.ToLower( path )
	}
	return
}

func normalDOI( doi string ) ( result string ) {
	result = strings.ToLower( strings.TrimSpace( doi ) )
	for _ , prefix := range []string{ "https://doi.org/" , "http://doi.org/" , "https://dx.doi.org/" , "http://dx.doi.org/" , "doi:" } {
		result = strings.TrimPrefix( result , prefix )
	}
	return
}

// normalTitle is a title with case, spacing and punctuation left out.
func normalTitle( title string ) ( result string ) {
	var built strings.Builder
	for _ , char := range strings.ToLower( title ) {
		if unicode.IsLetter( char ) || unicode.IsDigit( char ) { built.WriteRune( char ) }
	}
	result = built.String()
	return
}

func titleKey( item Item ) ( key string ) {
	// Not the type: RIS has no tag for some (a preprint comes back as a
	// journal article).
	key = normalTitle( Str( item , "title" ) ) + "|" + yearOf( Str( item , "date" ) )
	return
}

func sameChild( imported Item , stored Item ) ( same bool ) {
	if Str( imported , "itemType" ) == "note" {
		same = normalTitle( stripHTML( Str( imported , "note" ) ) ) == normalTitle( stripHTML( Str( stored , "note" ) ) )
		return
	}
	for _ , field := range []string{ "filename" , "url" , "path" , "title" } {
		if value := Str( imported , field ); value != "" && value == Str( stored , field ) {
			same = true
			return
		}
	}
	return
}

package models

import (
	bytes "bytes"
	json "encoding/json"
	errors "errors"
	fmt "fmt"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// Moving items (or collections) to other keys, in place: what fixes a library whose keys were
// made up by an import (an RDF export carries no Zotero keys) without
// sending the whole library again. Each move takes the item's record to its
// new key, can set fields on it (dateAdded, which no export carries), points
// its children (attachments, notes) at the new key, and leaves a tombstone
// under the old one, so a client following GET /api/library/changes sees
// the old key go and the new one come. All of one call happens in one
// transaction. The attachment's files in the WebDAV folder are moved by the
// caller (dav.Server.MoveAttachment), after.

// LibraryMove is one item to move. To may equal From: then it only sets
// fields.
type LibraryMove struct {
	From string
	To   string
	Set  map[string]any
}

// LibraryMoveResult says what happened to one move: Status is "moved",
// "set", "unchanged" or "refused", with Error saying why.
type LibraryMoveResult struct {
	From     string
	To       string
	Status   string
	Error    string
	Children int
}

var errMoveRefused = errors.New( "refused" )

// MoveLibraryItems applies moves to library_id's items. With dry_run nothing
// is written and the results say what would happen.
func MoveLibraryItems( store *db.Store , library_id uint64 , moves []LibraryMove , dry_run bool ) ( results []LibraryMoveResult , err error ) {
	results , err = moveLibrary( store , library_id , LibraryKindItem , moves , dry_run )
	return
}

// MoveLibraryCollections applies moves to library_id's collections: each
// collection goes to its new key, its subcollections point at it, and every
// item filed in it lists the new key in its collections. Children counts
// both.
func MoveLibraryCollections( store *db.Store , library_id uint64 , moves []LibraryMove , dry_run bool ) ( results []LibraryMoveResult , err error ) {
	results , err = moveLibrary( store , library_id , LibraryKindCollection , moves , dry_run )
	return
}

// moveLibrary moves records of kind. What points at a moved record changes
// with it: an item's attachments and notes (parentItem), a collection's
// subcollections (parentCollection) and the items filed in it
// (collections).
func moveLibrary( store *db.Store , library_id uint64 , kind string , moves []LibraryMove , dry_run bool ) ( results []LibraryMoveResult , err error ) {
	results = make( []LibraryMoveResult , len( moves ) )
	parent_field := "parentItem"
	if kind == LibraryKindCollection { parent_field = "parentCollection" }
	write := func( tx *db.Tx ) ( tx_err error ) {
		// Live records of both kinds: a collection's move reaches items.
		live := map[string]map[string]*LibraryRecord{ LibraryKindItem: {} , LibraryKindCollection: {} }
		for _ , each := range []string{ LibraryKindItem , LibraryKindCollection } {
			if each == LibraryKindCollection && kind != LibraryKindCollection { continue }
			{
				prefix := append( encryption.Uint64ToBytes( library_id ) , each[ 0 ] )
				tx_err = tx.ForEachPrefix( db.BucketLibrary , prefix , nil ,
					func() any { return &LibraryRecord{} } ,
					func( _ []byte , item any ) bool {
						if record := item.( *LibraryRecord ); record.Deleted == false && record.Kind == each { live[ each ][ record.Key ] = record }
						return true
					} )
				if tx_err != nil { return }
			}
		}
		own := live[ kind ]

		// Which moves can go: each key once on either side, the old key live,
		// the new one free. No chains or swaps: a key freed by another move
		// in the same call is still taken, so nothing is ever overwritten.
		from_seen , to_seen := map[string]bool{} , map[string]bool{}
		renamed := map[string]string{}
		for index , move := range moves {
			result := &results[ index ]
			result.From , result.To = move.From , move.To
			refuse := func( why string , args ...any ) { result.Status , result.Error = "refused" , fmt.Sprintf( why , args... ) }
			switch {
			case ZoteroKeyPattern.MatchString( move.From ) == false || ZoteroKeyPattern.MatchString( move.To ) == false:
				refuse( "keys are 8 of A-Z and 2-9" )
			case from_seen[ move.From ] || to_seen[ move.To ] || ( move.To != move.From && ( from_seen[ move.To ] || to_seen[ move.From ] ) ):
				refuse( "a key appears twice in this call" )
			case own[ move.From ] == nil:
				refuse( "no %s %s" , kind , move.From )
			case move.To != move.From && own[ move.To ] != nil:
				refuse( "%s is taken" , move.To )
			}
			if result.Status != "" { continue }
			from_seen[ move.From ] , to_seen[ move.To ] = true , true
			if move.To != move.From { renamed[ move.From ] = move.To }
		}

		// Every record that changes, by kind and the key it ends up under.
		type change struct {
			kind    string
			data    map[string]any
			version int64
		}
		changes := map[string]*change{}
		id := func( each string , key string ) string { return each + "/" + key }
		for index , move := range moves {
			if results[ index ].Status != "" { continue }
			record := own[ move.From ]
			data := map[string]any{}
			if tx_err = json.Unmarshal( record.Data , &data ); tx_err != nil { return }
			for field , value := range move.Set {
				if field == "key" || field == parent_field { continue }
				data[ field ] = value
			}
			data[ "key" ] = move.To
			changes[ id( kind , move.To ) ] = &change{ kind: kind , data: data , version: record.Version }
		}
		// found is the change for a record that points at a moved one.
		found := func( each string , key string , record *LibraryRecord ) ( result *change , found_err error ) {
			target := key
			if each == kind && renamed[ key ] != "" { target = renamed[ key ] }
			result = changes[ id( each , target ) ]
			if result == nil {
				data := map[string]any{}
				if found_err = json.Unmarshal( record.Data , &data ); found_err != nil { return }
				result = &change{ kind: each , data: data , version: record.Version }
				changes[ id( each , target ) ] = result
			}
			return
		}
		count := func( from string ) {
			for index := range results {
				if results[ index ].From == from && results[ index ].Status == "" { results[ index ].Children += 1 }
			}
		}
		for key , record := range own {
			parent := record.ParentKey
			if renamed[ parent ] == "" { continue }
			pointing , found_err := found( kind , key , record )
			if tx_err = found_err; tx_err != nil { return }
			pointing.data[ parent_field ] = renamed[ parent ]
			count( parent )
		}
		if kind == LibraryKindCollection {
			for key , record := range live[ LibraryKindItem ] {
				hit := false
				for _ , collection := range record.Collections { hit = hit || renamed[ collection ] != "" }
				if hit == false { continue }
				pointing , found_err := found( LibraryKindItem , key , record )
				if tx_err = found_err; tx_err != nil { return }
				listed , _ := pointing.data[ "collections" ].( []any )
				for index , value := range listed {
					if collection , ok := value.( string ); ok && renamed[ collection ] != "" {
						listed[ index ] = renamed[ collection ]
						count( collection )
					}
				}
				pointing.data[ "collections" ] = listed
			}
		}

		// Write the new records, then tombstones for the keys left behind.
		for index := range results {
			if results[ index ].Status != "" { continue }
			if results[ index ].To != results[ index ].From {
				results[ index ].Status = "moved"
			} else {
				results[ index ].Status = "set"
			}
		}
		for change_id , pending := range changes {
			key := change_id[ len( pending.kind ) + 1: ]
			raw , marshal_err := json.Marshal( pending.data )
			if marshal_err != nil {
				tx_err = marshal_err
				return
			}
			if tx_err = ValidLibraryData( key , raw ); tx_err != nil { return }
			stored := &LibraryRecord{}
			previous := uint64( 0 )
			if get_err := tx.Get( db.BucketLibrary , libraryRecordKey( library_id , pending.kind , key ) , stored ); get_err == nil {
				previous = stored.Rev
				if stored.Deleted == false && bytes.Equal( stored.Data , raw ) {
					for index := range results {
						if pending.kind == kind && results[ index ].To == key && results[ index ].Status == "set" { results[ index ].Status = "unchanged" }
					}
					continue
				}
			} else if errors.Is( get_err , db.ErrNotFound ) == false {
				tx_err = get_err
				return
			}
			if dry_run { continue }
			record := &LibraryRecord{ Kind: pending.kind , Key: key , Version: pending.version , Data: raw }
			record.describe()
			if tx_err = writeRecordTx( tx , library_id , record , previous ); tx_err != nil { return }
		}
		if dry_run { return }
		for from := range renamed {
			old := own[ from ]
			tombstone := &LibraryRecord{ Kind: kind , Key: from , Version: old.Version , Deleted: true }
			if tx_err = writeRecordTx( tx , library_id , tombstone , old.Rev ); tx_err != nil { return }
		}
		return
	}
	if dry_run {
		err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
			if tx_err = write( tx ); tx_err != nil { return }
			// Nothing of a dry run is kept.
			tx_err = errMoveRefused
			return
		} )
		if errors.Is( err , errMoveRefused ) { err = nil }
		return
	}
	if err = store.Write( write ); err != nil {
		results = nil
		return
	}
	libraryChanged( library_id , false )
	return
}

package models

import (
	bytes "bytes"
	json "encoding/json"
	errors "errors"
	fmt "fmt"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// Moving items to other keys, in place: what fixes a library whose keys were
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
	results = make( []LibraryMoveResult , len( moves ) )
	write := func( tx *db.Tx ) ( tx_err error ) {
		live := map[string]*LibraryRecord{}
		prefix := append( encryption.Uint64ToBytes( library_id ) , LibraryKindItem[ 0 ] )
		tx_err = tx.ForEachPrefix( db.BucketLibrary , prefix , nil ,
			func() any { return &LibraryRecord{} } ,
			func( _ []byte , item any ) bool {
				if record := item.( *LibraryRecord ); record.Deleted == false && record.Kind == LibraryKindItem { live[ record.Key ] = record }
				return true
			} )
		if tx_err != nil { return }

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
			case live[ move.From ] == nil:
				refuse( "no item %s" , move.From )
			case move.To != move.From && live[ move.To ] != nil:
				refuse( "%s is taken" , move.To )
			}
			if result.Status != "" { continue }
			from_seen[ move.From ] , to_seen[ move.To ] = true , true
			if move.To != move.From { renamed[ move.From ] = move.To }
		}

		// Every record that changes: the moved ones, and children of a moved
		// item, which point at it.
		type change struct {
			data    map[string]any
			version int64
			old     *LibraryRecord
		}
		changes := map[string]*change{}
		for index , move := range moves {
			if results[ index ].Status != "" { continue }
			record := live[ move.From ]
			data := map[string]any{}
			if tx_err = json.Unmarshal( record.Data , &data ); tx_err != nil { return }
			for field , value := range move.Set {
				if field == "key" || field == "parentItem" { continue }
				data[ field ] = value
			}
			data[ "key" ] = move.To
			changes[ move.To ] = &change{ data: data , version: record.Version , old: record }
		}
		for key , record := range live {
			parent := record.ParentKey
			if renamed[ parent ] == "" { continue }
			target := key
			if renamed[ key ] != "" { target = renamed[ key ] }
			found := changes[ target ]
			if found == nil {
				data := map[string]any{}
				if tx_err = json.Unmarshal( record.Data , &data ); tx_err != nil { return }
				found = &change{ data: data , version: record.Version , old: record }
				changes[ target ] = found
			}
			found.data[ "parentItem" ] = renamed[ parent ]
			for index := range results {
				if results[ index ].From == parent && results[ index ].Status == "" { results[ index ].Children += 1 }
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
		for key , found := range changes {
			raw , marshal_err := json.Marshal( found.data )
			if marshal_err != nil {
				tx_err = marshal_err
				return
			}
			if tx_err = ValidLibraryData( key , raw ); tx_err != nil { return }
			stored := &LibraryRecord{}
			previous := uint64( 0 )
			if get_err := tx.Get( db.BucketLibrary , libraryRecordKey( library_id , LibraryKindItem , key ) , stored ); get_err == nil {
				previous = stored.Rev
				if stored.Deleted == false && bytes.Equal( stored.Data , raw ) {
					for index := range results {
						if results[ index ].To == key && results[ index ].Status == "set" { results[ index ].Status = "unchanged" }
					}
					continue
				}
			} else if errors.Is( get_err , db.ErrNotFound ) == false {
				tx_err = get_err
				return
			}
			if dry_run { continue }
			record := &LibraryRecord{ Kind: LibraryKindItem , Key: key , Version: found.version , Data: raw }
			record.describe()
			if tx_err = writeRecordTx( tx , library_id , record , previous ); tx_err != nil { return }
		}
		if dry_run { return }
		for from := range renamed {
			old := live[ from ]
			tombstone := &LibraryRecord{ Kind: LibraryKindItem , Key: from , Version: old.Version , Deleted: true }
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

package models

import (
	errors "errors"
	regexp "regexp"
	sort "sort"
	strings "strings"
	time "time"
	unicode "unicode"

	norm "golang.org/x/text/unicode/norm"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// DAVAccount is one WebDAV library: a folder, the name it is reached by
// (<base>/<name>/), whether it may be used, and how much room it has. The
// metadata library (BucketLibrary*) and the attachment index are keyed by
// the same ID. An account may own several (webdav.max_libraries_per_user),
// say one per Zotero profile.
//
// ID is the library's own id. An account's first library, its primary, has
// ID == UserID, which is what every library made before there could be more
// than one has, so their folders and keys never moved. Later ones take ids
// from the users bucket's sequence, so no user made afterwards can share one.
//
// The files live under <app dir>/dav/<id>/, by id rather than by name, so
// renaming never moves a file and a name taken over later never reaches
// someone else's files. The name is a separate index (BucketDAVNames), so a
// WebDAV request finds its library in one read.
type DAVAccount struct {
	ID uint64 `json:"id"`
	// UserID is the account that owns the library.
	UserID  uint64 `json:"user_id"`
	Name    string `json:"name"`
	// Label is the owner's own note on what the library is ("Work").
	Label   string `json:"label,omitempty"`
	Enabled bool   `json:"enabled"`
	// NameLocked is an admin stopping the account changing its own WebDAV
	// name, and so its username. The first name is still the account's to
	// choose; false (every account made before this existed) leaves it free.
	NameLocked bool `json:"name_locked,omitempty"`
	// QuotaBytes is an admin's choice for this account; nil follows
	// webdav.default_quota_gb, and 0 is no limit.
	QuotaBytes *int64    `json:"quota_bytes,omitempty"`
	UsedBytes  int64     `json:"used_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type davNameRecord struct {
	LibraryID uint64 `json:"library_id"`
}

var (
	ErrDAVNameLocked   = errors.New( "an admin has locked your WebDAV name" )
	ErrDAVNameInvalid  = errors.New( "a WebDAV name is 2-32 characters: lowercase letters, digits and -, starting with a letter or digit" )
	ErrDAVNameTaken    = errors.New( "that WebDAV name is taken" )
	ErrDAVNameReserved = errors.New( "that WebDAV name is reserved" )
	ErrDAVLibraryLimit = errors.New( "you already have as many WebDAV libraries as you may" )
	ErrDAVPrimary      = errors.New( "your first WebDAV library cannot be deleted" )
	ErrDAVLabel        = errors.New( "a label is at most 60 characters" )
)

const davLabelMax = 60

var davNamePattern = regexp.MustCompile( `^[a-z0-9][a-z0-9-]{1,31}$` )

// Names that would read as something other than a person's folder, or that
// a reverse proxy in front might route elsewhere.
var davReservedNames = map[string]bool{
	"admin": true , "api": true , "dav": true , "webdav": true , "zotero": true , "login": true ,
	"logout": true , "join": true , "docs": true , "static": true , "vendor": true , "js": true ,
	"css": true , "health": true , "metrics": true , "mcp": true , "root": true , "www": true ,
	"well-known": true , "robots": true , "favicon": true , "owner": true , "system": true ,
}

// Primary is the account's first library, the one used when no library is
// named.
func ( account *DAVAccount ) Primary() ( result bool ) {
	result = account.ID == account.UserID
	return
}

// QuotaFor is the account's limit in bytes, 0 for none.
func ( account *DAVAccount ) QuotaFor( default_quota int64 ) ( quota int64 ) {
	quota = default_quota
	if account.QuotaBytes != nil { quota = *account.QuotaBytes }
	return
}

func ValidDAVName( name string ) ( err error ) {
	switch {
	case davNamePattern.MatchString( name ) == false:
		err = ErrDAVNameInvalid
	case davReservedNames[ name ]:
		err = ErrDAVNameReserved
	}
	return
}

// SuggestDAVName turns a display name into a name that passes ValidDAVName,
// or "" when nothing usable is left: "Dr. Zoë Lee" becomes "dr-zoe-lee".
func SuggestDAVName( display_name string ) ( name string ) {
	var built strings.Builder
	dash := false
	for _ , r := range norm.NFKD.String( strings.ToLower( display_name ) ) {
		switch {
		case unicode.Is( unicode.Mn , r ):
			continue
		case ( r >= 'a' && r <= 'z' ) || ( r >= '0' && r <= '9' ):
			built.WriteRune( r )
			dash = false
		case built.Len() > 0 && dash == false:
			built.WriteByte( '-' )
			dash = true
		}
	}
	name = strings.Trim( built.String() , "-" )
	if len( name ) > 32 { name = strings.Trim( name[ :32 ] , "-" ) }
	if ValidDAVName( name ) != nil { name = "" }
	return
}

// GetDAVAccount is one library by id. An account's primary has its user id.
func GetDAVAccount( store *db.Store , library_id uint64 ) ( account *DAVAccount , err error ) {
	account = &DAVAccount{}
	if err = store.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account ); err != nil {
		account = nil
	}
	return
}

// DAVAccountByName finds an account by its WebDAV name. db.ErrNotFound when
// no account has it.
func DAVAccountByName( store *db.Store , name string ) ( account *DAVAccount , err error ) {
	record := &davNameRecord{}
	if err = store.Get( db.BucketDAVNames , []byte( name ) , record ); err != nil { return }
	account , err = GetDAVAccount( store , record.LibraryID )
	if err == nil && account.Name != name {
		account , err = nil , db.ErrNotFound
	}
	return
}

// ClaimDAVName gives an account's primary library its WebDAV name, making it
// the first time, or renames it. RenameDAVLibrary does the same for any of
// the account's libraries. The old name stops working at once and becomes
// free. enabled is whether a new library may be used
// (webdav.enabled_by_default); a rename keeps whatever an admin set. A rename
// of a locked name is refused with ErrDAVNameLocked unless locked_ok (the
// admin CLI, which may).
func ClaimDAVName( store *db.Store , user_id uint64 , name string , enabled bool , locked_ok bool ) ( account *DAVAccount , created bool , err error ) {
	account , created , err = RenameDAVLibrary( store , user_id , user_id , name , enabled , locked_ok )
	return
}

// RenameDAVLibrary names one of user_id's libraries. library_id == user_id
// may make the primary; any other library has to exist already
// (CreateDAVLibrary makes them), and someone else's is db.ErrNotFound.
func RenameDAVLibrary( store *db.Store , user_id uint64 , library_id uint64 , name string , enabled bool , locked_ok bool ) ( account *DAVAccount , created bool , err error ) {
	name = strings.TrimSpace( name )
	if err = ValidDAVName( name ); err != nil { return }
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		account , created , tx_err = claimTx( tx , user_id , library_id , name , enabled , locked_ok , library_id == user_id )
		return
	} )
	if err != nil {
		account , created = nil , false
	}
	return
}

// claimTx names library_id, making it when may_create and it does not exist.
func claimTx( tx *db.Tx , user_id uint64 , library_id uint64 , name string , enabled bool , locked_ok bool , may_create bool ) ( account *DAVAccount , created bool , err error ) {
	user := &User{}
	if err = tx.Get( db.BucketUsers , encryption.Uint64ToBytes( user_id ) , user ); err != nil { return }
	holder := &davNameRecord{}
	switch get_err := tx.Get( db.BucketDAVNames , []byte( name ) , holder ); {
	case get_err == nil && holder.LibraryID != library_id:
		err = ErrDAVNameTaken
		return
	case get_err != nil && errors.Is( get_err , db.ErrNotFound ) == false:
		err = get_err
		return
	}
	now := time.Now().UTC()
	account = &DAVAccount{}
	switch get_err := tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account ); {
	case errors.Is( get_err , db.ErrNotFound ) && may_create:
		account = &DAVAccount{ ID: library_id , UserID: user_id , Enabled: enabled , CreatedAt: now }
		created = true
	case get_err != nil:
		err = get_err
		return
	case account.UserID != user_id:
		err = db.ErrNotFound
		return
	}
	if account.Name != "" && account.Name != name {
		if account.NameLocked && locked_ok == false {
			err = ErrDAVNameLocked
			return
		}
		if err = tx.Delete( db.BucketDAVNames , []byte( account.Name ) ); err != nil { return }
	}
	account.Name = name
	account.UpdatedAt = now
	if err = tx.Put( db.BucketDAVNames , []byte( name ) , &davNameRecord{ LibraryID: library_id } ); err != nil { return }
	err = tx.Put( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account )
	return
}

// CreateDAVLibrary makes a library for user_id. The account's first one is
// its primary (id == user_id); later ones take a fresh id. limit is
// webdav.max_libraries_per_user, 0 for none.
func CreateDAVLibrary( store *db.Store , user_id uint64 , name string , label string , enabled bool , limit int ) ( account *DAVAccount , err error ) {
	name = strings.TrimSpace( name )
	label = strings.TrimSpace( label )
	if err = ValidDAVName( name ); err != nil { return }
	if len( []rune( label ) ) > davLabelMax {
		err = ErrDAVLabel
		return
	}
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		owned , tx_err := librariesOfTx( tx , user_id )
		if tx_err != nil { return }
		if limit > 0 && len( owned ) >= limit {
			tx_err = ErrDAVLibraryLimit
			return
		}
		library_id := user_id
		if len( owned ) > 0 {
			if library_id , tx_err = tx.NextSequence( db.BucketUsers ); tx_err != nil { return }
		}
		if account , _ , tx_err = claimTx( tx , user_id , library_id , name , enabled , false , true ); tx_err != nil { return }
		if label != "" {
			account.Label = label
			tx_err = tx.Put( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account )
		}
		return
	} )
	if err != nil { account = nil }
	return
}

// SetDAVLabel changes one of user_id's libraries' label.
func SetDAVLabel( store *db.Store , user_id uint64 , library_id uint64 , label string ) ( account *DAVAccount , err error ) {
	label = strings.TrimSpace( label )
	if len( []rune( label ) ) > davLabelMax {
		err = ErrDAVLabel
		return
	}
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		account = &DAVAccount{}
		if tx_err = tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account ); tx_err != nil { return }
		if account.UserID != user_id {
			tx_err = db.ErrNotFound
			return
		}
		account.Label = label
		account.UpdatedAt = time.Now().UTC()
		tx_err = tx.Put( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account )
		return
	} )
	if err != nil { account = nil }
	return
}

// DeleteDAVLibrary removes one of user_id's libraries other than the
// primary: its name, app passwords, attachment index and metadata. Its files
// go with the next dav-reconcile run, which removes a folder no library has.
func DeleteDAVLibrary( store *db.Store , user_id uint64 , library_id uint64 ) ( account *DAVAccount , err error ) {
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		account = &DAVAccount{}
		if tx_err = tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account ); tx_err != nil { return }
		if account.UserID != user_id {
			tx_err = db.ErrNotFound
			return
		}
		if account.Primary() {
			tx_err = ErrDAVPrimary
			return
		}
		tx_err = deleteLibraryTx( tx , account )
		return
	} )
	if err != nil { account = nil }
	return
}

// ListDAVLibrariesForOwner is user_id's libraries, the primary first and the
// rest by name.
func ListDAVLibrariesForOwner( store *db.Store , user_id uint64 ) ( accounts []*DAVAccount , err error ) {
	accounts = []*DAVAccount{}
	err = store.ForEach( db.BucketDAVAccounts ,
		func() any { return &DAVAccount{} } ,
		func( key []byte , item any ) bool {
			if account := item.( *DAVAccount ); account.UserID == user_id {
				accounts = append( accounts , account )
			}
			return true
		} )
	sortLibraries( accounts )
	return
}

func librariesOfTx( tx *db.Tx , user_id uint64 ) ( accounts []*DAVAccount , err error ) {
	accounts = []*DAVAccount{}
	err = tx.ForEach( db.BucketDAVAccounts ,
		func() any { return &DAVAccount{} } ,
		func( key []byte , item any ) bool {
			if account := item.( *DAVAccount ); account.UserID == user_id {
				accounts = append( accounts , account )
			}
			return true
		} )
	sortLibraries( accounts )
	return
}

func sortLibraries( accounts []*DAVAccount ) {
	sort.SliceStable( accounts , func( i int , j int ) bool {
		if accounts[ i ].Primary() != accounts[ j ].Primary() { return accounts[ i ].Primary() }
		return accounts[ i ].Name < accounts[ j ].Name
	} )
}

// SetDAVAccess is an admin switching a library on or off, locking or
// unlocking its name, or setting its quota: clear_quota puts it back on the
// default. It follows the ladder every account action does (mayActOnIDTx),
// against the library's owner.
func SetDAVAccess( store *db.Store , actor_id uint64 , library_id uint64 , enabled *bool , name_locked *bool , quota *int64 , clear_quota bool ) ( account *DAVAccount , err error ) {
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		account = &DAVAccount{}
		if tx_err = tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account ); tx_err != nil { return }
		if tx_err = mayActOnIDTx( tx , actor_id , account.UserID ); tx_err != nil { return }
		if enabled != nil { account.Enabled = *enabled }
		if name_locked != nil { account.NameLocked = *name_locked }
		switch {
		case clear_quota:
			account.QuotaBytes = nil
		case quota != nil:
			value := *quota
			account.QuotaBytes = &value
		}
		account.UpdatedAt = time.Now().UTC()
		tx_err = tx.Put( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , account )
		return
	} )
	if err != nil { account = nil }
	return
}

// SetDAVUsage records how much a library's folder holds.
func SetDAVUsage( store *db.Store , library_id uint64 , used int64 ) ( err error ) {
	err = store.UpdateValue( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) ,
		func() any { return &DAVAccount{} } ,
		func( item any ) ( mutate_err error ) {
			item.( *DAVAccount ).UsedBytes = max( used , 0 )
			return
		} )
	return
}

// ListDAVAccounts is every library, by name.
func ListDAVAccounts( store *db.Store ) ( accounts []*DAVAccount , err error ) {
	accounts = []*DAVAccount{}
	err = store.ForEach( db.BucketDAVAccounts ,
		func() any { return &DAVAccount{} } ,
		func( key []byte , item any ) bool {
			accounts = append( accounts , item.( *DAVAccount ) )
			return true
		} )
	sort.Slice( accounts , func( i int , j int ) bool { return accounts[ i ].Name < accounts[ j ].Name } )
	return
}

// deleteDAVForUserTx removes everything WebDAV keeps in the database for an
// account: every library it owns, with its name, app passwords, attachment
// index and metadata. DeleteUser calls it. The files go with the next
// dav-reconcile run, which removes a folder no library has.
func deleteDAVForUserTx( tx *db.Tx , user_id uint64 ) ( err error ) {
	owned , err := librariesOfTx( tx , user_id )
	if err != nil { return }
	for _ , account := range owned {
		if err = deleteLibraryTx( tx , account ); err != nil { return }
	}
	// Passwords left from a library already gone, and the primary's
	// metadata when it never had a WebDAV name.
	if _ , err = tx.DeleteWhere( db.BucketDAVPasswords ,
		func() any { return &DAVPassword{} } ,
		func( key []byte , item any ) bool { return item.( *DAVPassword ).UserID == user_id } ); err != nil { return }
	err = deleteLibraryDataTx( tx , user_id )
	return
}

// deleteLibraryTx removes one library's records.
func deleteLibraryTx( tx *db.Tx , account *DAVAccount ) ( err error ) {
	if err = tx.Delete( db.BucketDAVNames , []byte( account.Name ) ); err != nil { return }
	if err = tx.Delete( db.BucketDAVAccounts , encryption.Uint64ToBytes( account.ID ) ); err != nil { return }
	if _ , err = tx.DeleteWhere( db.BucketDAVPasswords ,
		func() any { return &DAVPassword{} } ,
		func( key []byte , item any ) bool { return item.( *DAVPassword ).LibraryID == account.ID } ); err != nil { return }
	err = deleteLibraryDataTx( tx , account.ID )
	return
}

// deleteLibraryDataTx removes the attachment index and metadata kept under
// one library id.
func deleteLibraryDataTx( tx *db.Tx , library_id uint64 ) ( err error ) {
	prefix := encryption.Uint64ToBytes( library_id )
	for _ , bucket := range []string{ db.BucketDAVAttachments , db.BucketLibrary , db.BucketLibraryRevs } {
		if err = tx.DeletePrefix( bucket , prefix ); err != nil { return }
	}
	err = tx.Delete( db.BucketLibraryMeta , prefix )
	return
}

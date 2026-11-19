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

// DAVAccount is someone's WebDAV folder: the name it is reached by
// (<base>/<name>/), whether they may use it, and how much room it has.
//
// The files live under <app dir>/dav/<user id>/, by id rather than by name,
// so renaming never moves a file and a name taken over later never reaches
// someone else's files. The name is a separate index (BucketDAVNames), so a
// WebDAV request finds its account in one read.
type DAVAccount struct {
	UserID  uint64 `json:"user_id"`
	Name    string `json:"name"`
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
	UserID uint64 `json:"user_id"`
}

var (
	ErrDAVNameLocked   = errors.New( "an admin has locked your WebDAV name" )
	ErrDAVNameInvalid  = errors.New( "a WebDAV name is 2-32 characters: lowercase letters, digits and -, starting with a letter or digit" )
	ErrDAVNameTaken    = errors.New( "that WebDAV name is taken" )
	ErrDAVNameReserved = errors.New( "that WebDAV name is reserved" )
)

var davNamePattern = regexp.MustCompile( `^[a-z0-9][a-z0-9-]{1,31}$` )

// Names that would read as something other than a person's folder, or that
// a reverse proxy in front might route elsewhere.
var davReservedNames = map[string]bool{
	"admin": true , "api": true , "dav": true , "webdav": true , "zotero": true , "login": true ,
	"logout": true , "join": true , "docs": true , "static": true , "vendor": true , "js": true ,
	"css": true , "health": true , "metrics": true , "mcp": true , "root": true , "www": true ,
	"well-known": true , "robots": true , "favicon": true , "owner": true , "system": true ,
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

func GetDAVAccount( store *db.Store , user_id uint64 ) ( account *DAVAccount , err error ) {
	account = &DAVAccount{}
	if err = store.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) , account ); err != nil {
		account = nil
	}
	return
}

// DAVAccountByName finds an account by its WebDAV name. db.ErrNotFound when
// no account has it.
func DAVAccountByName( store *db.Store , name string ) ( account *DAVAccount , err error ) {
	record := &davNameRecord{}
	if err = store.Get( db.BucketDAVNames , []byte( name ) , record ); err != nil { return }
	account , err = GetDAVAccount( store , record.UserID )
	if err == nil && account.Name != name {
		account , err = nil , db.ErrNotFound
	}
	return
}

// ClaimDAVName gives an account its WebDAV name, making the account the first
// time, or renames it. The old name stops working at once and becomes free.
// enabled is whether a new account may use WebDAV (webdav.enabled_by_default);
// a rename keeps whatever an admin set. A rename of a locked name is refused
// with ErrDAVNameLocked unless locked_ok (the admin CLI, which may).
func ClaimDAVName( store *db.Store , user_id uint64 , name string , enabled bool , locked_ok bool ) ( account *DAVAccount , created bool , err error ) {
	name = strings.TrimSpace( name )
	if err = ValidDAVName( name ); err != nil { return }
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		user := &User{}
		if tx_err = tx.Get( db.BucketUsers , encryption.Uint64ToBytes( user_id ) , user ); tx_err != nil { return }
		holder := &davNameRecord{}
		switch get_err := tx.Get( db.BucketDAVNames , []byte( name ) , holder ); {
		case get_err == nil && holder.UserID != user_id:
			tx_err = ErrDAVNameTaken
			return
		case get_err != nil && errors.Is( get_err , db.ErrNotFound ) == false:
			tx_err = get_err
			return
		}
		now := time.Now().UTC()
		account = &DAVAccount{}
		switch get_err := tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) , account ); {
		case errors.Is( get_err , db.ErrNotFound ):
			account = &DAVAccount{ UserID: user_id , Enabled: enabled , CreatedAt: now }
			created = true
		case get_err != nil:
			tx_err = get_err
			return
		}
		if account.Name != "" && account.Name != name {
			if account.NameLocked && locked_ok == false {
				tx_err = ErrDAVNameLocked
				return
			}
			if tx_err = tx.Delete( db.BucketDAVNames , []byte( account.Name ) ); tx_err != nil { return }
		}
		account.Name = name
		account.UpdatedAt = now
		if tx_err = tx.Put( db.BucketDAVNames , []byte( name ) , &davNameRecord{ UserID: user_id } ); tx_err != nil { return }
		tx_err = tx.Put( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) , account )
		return
	} )
	if err != nil {
		account , created = nil , false
	}
	return
}

// SetDAVAccess is an admin switching an account's WebDAV on or off, locking
// or unlocking its name, or setting its quota: clear_quota puts it back on the
// default. It follows the ladder every account action does (mayActOnIDTx).
func SetDAVAccess( store *db.Store , actor_id uint64 , user_id uint64 , enabled *bool , name_locked *bool , quota *int64 , clear_quota bool ) ( account *DAVAccount , err error ) {
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		if tx_err = mayActOnIDTx( tx , actor_id , user_id ); tx_err != nil { return }
		account = &DAVAccount{}
		if tx_err = tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) , account ); tx_err != nil { return }
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
		tx_err = tx.Put( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) , account )
		return
	} )
	if err != nil { account = nil }
	return
}

// SetDAVUsage records how much an account's folder holds.
func SetDAVUsage( store *db.Store , user_id uint64 , used int64 ) ( err error ) {
	err = store.UpdateValue( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) ,
		func() any { return &DAVAccount{} } ,
		func( item any ) ( mutate_err error ) {
			item.( *DAVAccount ).UsedBytes = max( used , 0 )
			return
		} )
	return
}

// ListDAVAccounts is every account with a WebDAV name, by name.
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
// account: its name, its app passwords, its attachment index and its library.
// DeleteUser calls it. The files go with the next dav-reconcile run, which
// removes the folder of an account that no longer exists.
func deleteDAVForUserTx( tx *db.Tx , user_id uint64 ) ( err error ) {
	account := &DAVAccount{}
	switch get_err := tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) , account ); {
	case get_err == nil:
		if err = tx.Delete( db.BucketDAVNames , []byte( account.Name ) ); err != nil { return }
		if err = tx.Delete( db.BucketDAVAccounts , encryption.Uint64ToBytes( user_id ) ); err != nil { return }
	case errors.Is( get_err , db.ErrNotFound ) == false:
		err = get_err
		return
	}
	if _ , err = tx.DeleteWhere( db.BucketDAVPasswords ,
		func() any { return &DAVPassword{} } ,
		func( key []byte , item any ) bool { return item.( *DAVPassword ).UserID == user_id } ); err != nil { return }
	prefix := encryption.Uint64ToBytes( user_id )
	for _ , bucket := range []string{ db.BucketDAVAttachments , db.BucketLibrary , db.BucketLibraryRevs } {
		if err = tx.DeletePrefix( bucket , prefix ); err != nil { return }
	}
	err = tx.Delete( db.BucketLibraryMeta , prefix )
	return
}

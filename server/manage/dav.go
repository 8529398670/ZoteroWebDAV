package manage

import (
	errors "errors"
	flag "flag"
	fmt "fmt"
	os "os"
	strings "strings"

	config "zoterowebdav/server/config"
	control "zoterowebdav/server/control"
	dav "zoterowebdav/server/dav"
	db "zoterowebdav/server/db"
	models "zoterowebdav/server/models"
)

func ( b *controlBackend ) ListDAV() ( accounts []control.DAVAccountResponse , err error ) {
	accounts , err = b.client.ListDAV()
	return
}

func ( b *controlBackend ) SetDAVName( user_id uint64 , name string ) ( account control.DAVAccountResponse , err error ) {
	account , err = b.client.SetDAVName( user_id , name )
	return
}

func ( b *controlBackend ) CreateDAVPassword( user_id uint64 , name string , days int ) ( password control.DAVPasswordResponse , err error ) {
	password , err = b.client.CreateDAVPassword( user_id , name , days )
	return
}

func ( b *directBackend ) ListDAV() ( accounts []control.DAVAccountResponse , err error ) {
	accounts , err = control.ListDAV( b.store )
	return
}

func ( b *directBackend ) SetDAVName( user_id uint64 , name string ) ( account control.DAVAccountResponse , err error ) {
	account , err = control.SetDAVName( b.store , user_id , name , b.cfg.WebDAV.EnabledByDefault )
	return
}

func ( b *directBackend ) CreateDAVPassword( user_id uint64 , name string , days int ) ( password control.DAVPasswordResponse , err error ) {
	password , err = control.CreateDAVPassword( b.store , user_id , name , days )
	return
}

func commandDAVList( back backend ) ( err error ) {
	accounts , err := back.ListDAV()
	if err != nil { return }
	if len( accounts ) == 0 {
		fmt.Println( "no account has a WebDAV name yet" )
		return
	}
	fmt.Printf( "%-6s %-24s %-24s %-8s %12s %12s\n" , "ID" , "ACCOUNT" , "WEBDAV NAME" , "ACCESS" , "USED MB" , "QUOTA" )
	for _ , account := range accounts {
		access := "on"
		if account.Enabled == false { access = "off" }
		quota := "default"
		if account.QuotaBytes != nil {
			quota = fmt.Sprintf( "%d GB" , *account.QuotaBytes / 1024 / 1024 / 1024 )
			if *account.QuotaBytes == 0 { quota = "none" }
		}
		fmt.Printf( "%-6d %-24s %-24s %-8s %12.1f %12s\n" , account.UserID , account.DisplayName , account.Name , access , float64( account.UsedBytes ) / 1024 / 1024 , quota )
	}
	return
}

func commandDAVSetName( back backend , cfg *config.Config , arguments []string ) ( err error ) {
	set := flag.NewFlagSet( "dav-set-name" , flag.ExitOnError )
	user_id := set.Uint64( "user-id" , 0 , "id of the account" )
	name := set.String( "name" , "" , "the WebDAV name: 2-32 lowercase letters, digits and -" )
	set.Parse( arguments )
	if *user_id == 0 || models.ValidDAVName( *name ) != nil {
		err = fmt.Errorf( "-user-id and a valid -name are required (%v)" , models.ErrDAVNameInvalid )
		return
	}
	account , err := back.SetDAVName( *user_id , *name )
	if err != nil { return }
	if root_err := os.MkdirAll( dav.ZoteroFolder( cfg.WebDAV.Dir , account.UserID ) , 0o700 ); root_err != nil {
		fmt.Fprintf( os.Stderr , "warning: could not create the folder yet: %v\n" , root_err )
	}
	fmt.Printf( "#%d %q is %s at %s/%s/ (Zotero: enter %s/%s)\n" , account.UserID , account.DisplayName , account.Name , cfg.WebDAV.BaseURL() , account.Name , strings.TrimPrefix( strings.TrimPrefix( cfg.WebDAV.BaseURL() , "https://" ) , "http://" ) , account.Name )
	return
}

func commandDAVCreatePassword( back backend , cfg *config.Config , arguments []string ) ( err error ) {
	set := flag.NewFlagSet( "dav-create-password" , flag.ExitOnError )
	user_id := set.Uint64( "user-id" , 0 , "id of the account" )
	name := set.String( "name" , "" , "what will use it, e.g. \"Zotero laptop\"" )
	days := set.Int( "days" , 0 , "lifetime in days; 0 never expires" )
	set.Parse( arguments )
	if *user_id == 0 || strings.TrimSpace( *name ) == "" {
		err = errors.New( "-user-id and -name are required" )
		return
	}
	password , err := back.CreateDAVPassword( *user_id , strings.TrimSpace( *name ) , *days )
	if err != nil { return }
	fmt.Printf( "App password %q for WebDAV name %s (shown once, store it now):\n\n  user name: %s\n  password:  %s\n  address:   %s/%s\n\n" , password.Name , password.Username , password.Username , password.Credential , cfg.WebDAV.BaseURL() , password.Username )
	return
}

// commandDAVConvert rewrites every WebDAV file encrypted, or decrypted. It
// needs the server stopped: it holds the database for the master key, and a
// file must not be written while it is being rewritten.
func commandDAVConvert( cfg *config.Config , arguments []string ) ( err error ) {
	set := flag.NewFlagSet( "dav-convert" , flag.ExitOnError )
	to := set.String( "to" , "" , "encrypted or plain" )
	yes := set.Bool( "yes" , false , "really rewrite every file" )
	set.Parse( arguments )
	if *to != "encrypted" && *to != "plain" {
		err = errors.New( "-to must be encrypted or plain" )
		return
	}
	if *yes == false {
		err = errors.New( "this rewrites every WebDAV file -- add -yes to go ahead" )
		return
	}
	store , err := db.Open( cfg )
	if err != nil {
		if db.IsLocked( err ) { err = errors.New( "the server is running -- stop it first" ) }
		return
	}
	defer store.Close()
	master , err := models.DAVMasterKey( store , cfg.SecretKey )
	if err != nil { return }
	result , err := dav.Convert( cfg.WebDAV.Dir , master , *to == "encrypted" )
	fmt.Printf( "%d files rewritten as %s, %d already were\n" , result.Converted , *to , result.Skipped )
	if err == nil && ( *to == "encrypted" ) != cfg.WebDAV.EncryptAtRest {
		fmt.Printf( "note: webdav.encrypt_at_rest is %v, so new files will still be written %s\n" , cfg.WebDAV.EncryptAtRest , map[bool]string{ true: "encrypted" , false: "plain" }[ cfg.WebDAV.EncryptAtRest ] )
	}
	return
}

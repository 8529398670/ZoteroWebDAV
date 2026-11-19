package routes

import (
	errors "errors"
	io "io"
	os "os"
	path "path"
	strings "strings"
	time "time"

	fiber "github.com/gofiber/fiber/v3"

	apidoc "zoterowebdav/server/apidoc"
	dav "zoterowebdav/server/dav"
	db "zoterowebdav/server/db"
	logs "zoterowebdav/server/logs"
	models "zoterowebdav/server/models"
	security "zoterowebdav/server/security"
)

// The WebDAV screen: your WebDAV name and the address to give Zotero, the app
// passwords WebDAV clients sign in with, how full your folder is, and the
// folder itself (browse, download, delete into a trash, put back). Admins set
// who may use WebDAV and how much room they get.
//
// Everything here is the caller's own folder. Nobody, admins included, can
// list, read or delete anyone else's files through these routes: an admin
// sees names and usage, never contents.
//
// App passwords are credentials, so they are made and managed from a browser
// session only (SESSION_ONLY), like API keys: a leaked key cannot mint itself
// a way into WebDAV.

// ---------------------------------------------------------------------------
// libraries
// ---------------------------------------------------------------------------

// libraryQuery names one of the caller's WebDAV libraries. Every WebDAV and
// library route takes it; absent means the primary, so a client that knows
// nothing of libraries keeps working on the one it always had.
type libraryQuery struct {
	Library string `json:"library,omitempty" query:"library" jsonschema:"maxLength=32" jsonschema_description:"Which of your WebDAV libraries, by its WebDAV name (GET /api/webdav/libraries). Absent: your first one." error:"library must be a WebDAV name"`
}

// libraryOf resolves a library name to its id, for the caller. name comes
// from the request (a bulk item carries its own); an empty one falls back to
// the URL's library parameter, then to the primary, whose id is the user's.
// A library that is not the caller's is 404, the same as one that does not
// exist.
func ( handlers *Handlers ) libraryOf( c fiber.Ctx , name string ) ( library_id uint64 , err error ) {
	account , err := handlers.libraryAccount( c , name )
	if err != nil { return }
	library_id = security.UserFrom( c ).ID
	if account != nil { library_id = account.ID }
	return
}

// libraryAccount is libraryOf's library itself: nil (and no error) for a
// primary that has no WebDAV name yet.
func ( handlers *Handlers ) libraryAccount( c fiber.Ctx , name string ) ( account *models.DAVAccount , err error ) {
	user := security.UserFrom( c )
	name = strings.ToLower( strings.TrimSpace( name ) )
	if name == "" { name = strings.ToLower( strings.TrimSpace( c.Query( "library" ) ) ) }
	var get_err error
	if name == "" {
		account , get_err = models.GetDAVAccount( handlers.Store , user.ID )
		if errors.Is( get_err , db.ErrNotFound ) {
			account , get_err = nil , nil
		}
	} else {
		account , get_err = models.DAVAccountByName( handlers.Store , name )
		if get_err == nil && account.UserID != user.ID {
			account , get_err = nil , db.ErrNotFound
		}
		if errors.Is( get_err , db.ErrNotFound ) {
			err = refuse( fiber.StatusNotFound , "you have no WebDAV library called " + name )
			return
		}
	}
	err = get_err
	return
}

// RequireWebDAV hides every WebDAV route while webdav.enabled is off.
func ( handlers *Handlers ) RequireWebDAV( c fiber.Ctx ) ( err error ) {
	if handlers.cfg().WebDAV.Enabled == false || handlers.DAV == nil {
		err = notFound( c , "webdav is switched off on this server" )
		return
	}
	err = c.Next()
	return
}

type webdavAccountRow struct {
	ID         uint64 `json:"id" jsonschema_description:"The library's id. An account's first library has the account's own id."`
	Name       string `json:"name"`
	Label      string `json:"label,omitempty" jsonschema_description:"The owner's note on what it is for."`
	Primary    bool   `json:"primary" jsonschema_description:"true for the account's first library, the one used when no library is named. It cannot be deleted."`
	Enabled    bool   `json:"enabled"`
	NameLocked bool   `json:"name_locked" jsonschema_description:"true when an admin has stopped this account changing its WebDAV name."`
	QuotaBytes int64  `json:"quota_bytes" jsonschema_description:"0 means no limit."`
	QuotaSet   bool   `json:"quota_set" jsonschema_description:"true when an admin set this account's quota; false when it follows webdav.default_quota_gb."`
	UsedBytes  int64  `json:"used_bytes"`
}

func ( webdavAccountRow ) APIName() ( name string ) { name = "WebDAVAccount"; return }

type webdavMeReply struct {
	Account       *webdavAccountRow `json:"account" jsonschema_description:"null until you choose a WebDAV name."`
	SuggestedName string            `json:"suggested_name,omitempty" jsonschema_description:"A name made from your display name, when you have none yet."`
	BaseURL       string            `json:"base_url" jsonschema_description:"What every WebDAV address here starts with."`
	URL           string            `json:"url,omitempty" jsonschema_description:"Your folder: what to give a WebDAV client. Zotero adds /zotero/ itself."`
	ZoteroURL     string            `json:"zotero_url,omitempty" jsonschema_description:"The folder Zotero syncs to: url + /zotero/."`
	Scheme        string            `json:"scheme" jsonschema:"enum=http,enum=https"`
	HostPath      string            `json:"host_path,omitempty" jsonschema_description:"url without its scheme: what goes in Zotero's URL box, beside its http/https choice."`
	EncryptAtRest bool              `json:"encrypt_at_rest"`
	MaxFileBytes  int64             `json:"max_file_bytes"`
	Libraries     []webdavLibraryRow `json:"libraries" jsonschema_description:"All your WebDAV libraries, the first one first. account is the one this reply is about."`
	MaxLibraries  int               `json:"max_libraries" jsonschema_description:"How many libraries you may have; 0 is no limit."`
	AutoURL       bool              `json:"auto_url" jsonschema_description:"true when webdav.public_url is empty: the WebDAV screen then builds the address from the page's own scheme and host, plus prefix (or direct_port when the page was opened on a port of its own)."`
	Prefix        string            `json:"prefix" jsonschema_description:"webdav.prefix: the path in front of every WebDAV name, or empty."`
	DirectPort    int               `json:"direct_port" jsonschema_description:"The port WebDAV listens on for clients that reach it without a proxy."`
}

// webdavLibraryRow is one library with its address.
type webdavLibraryRow struct {
	webdavAccountRow
	URL      string `json:"url" jsonschema_description:"What to give a WebDAV client. Zotero adds /zotero/ itself."`
	HostPath string `json:"host_path" jsonschema_description:"url without its scheme, for Zotero's URL box."`
}

func ( webdavLibraryRow ) APIName() ( name string ) { name = "WebDAVLibrary"; return }

func ( handlers *Handlers ) webdavLibraryRow( account *models.DAVAccount ) ( row webdavLibraryRow ) {
	url := handlers.cfg().WebDAV.BaseURL() + "/" + account.Name
	row = webdavLibraryRow{ webdavAccountRow: *handlers.webdavAccountRow( account ) , URL: url , HostPath: strings.TrimPrefix( strings.TrimPrefix( url , "https://" ) , "http://" ) }
	return
}

func ( handlers *Handlers ) webdavAccountRow( account *models.DAVAccount ) ( row *webdavAccountRow ) {
	cfg := handlers.cfg()
	row = &webdavAccountRow{
		ID:         account.ID,
		Name:       account.Name,
		Label:      account.Label,
		Primary:    account.Primary(),
		Enabled:    account.Enabled,
		NameLocked: account.NameLocked,
		QuotaBytes: account.QuotaFor( cfg.WebDAV.DefaultQuotaBytes ),
		QuotaSet:   account.QuotaBytes != nil,
		UsedBytes:  handlers.DAV.Usage( account.ID ),
	}
	return
}

// webdavMe is the WebDAV screen's state, about account (nil: the primary,
// which has no name yet).
func ( handlers *Handlers ) webdavMe( user *models.User , account *models.DAVAccount ) ( reply *webdavMeReply , err error ) {
	cfg := handlers.cfg()
	base := cfg.WebDAV.BaseURL()
	reply = &webdavMeReply{ BaseURL: base , EncryptAtRest: cfg.WebDAV.EncryptAtRest , MaxFileBytes: cfg.WebDAV.MaxFileBytes , Scheme: "http" , MaxLibraries: cfg.WebDAV.MaxLibraries , Libraries: []webdavLibraryRow{} , AutoURL: cfg.WebDAV.PublicURL == "" , Prefix: cfg.WebDAV.Prefix , DirectPort: cfg.WebDAV.DirectPort }
	if strings.HasPrefix( base , "https://" ) { reply.Scheme = "https" }
	owned , err := models.ListDAVLibrariesForOwner( handlers.Store , user.ID )
	if err != nil { return }
	for _ , library := range owned {
		reply.Libraries = append( reply.Libraries , handlers.webdavLibraryRow( library ) )
	}
	if account == nil {
		reply.SuggestedName = models.SuggestDAVName( user.DisplayName )
		return
	}
	reply.Account = handlers.webdavAccountRow( account )
	reply.URL = base + "/" + account.Name
	reply.ZoteroURL = reply.URL + "/zotero/"
	reply.HostPath = strings.TrimPrefix( strings.TrimPrefix( reply.URL , "https://" ) , "http://" )
	return
}

// getWebDAV is your WebDAV folder: its name, the address to give a client,
// and how full it is.
//
// @route("GET /api/webdav", summary="Your WebDAV folder",
//     tag="WebDAV", feature="webdav", errors=(404,),
//     description="One of your WebDAV libraries (library, default your first): its name, the address to give "
//         "Zotero or another client, and how full it is, plus the list of all of them. account is null "
//         "until you choose a name (POST /api/webdav/name).",
// )
func ( handlers *Handlers ) getWebDAV( c fiber.Ctx , in *libraryQuery ) ( reply *webdavMeReply , err error ) {
	account , err := handlers.libraryAccount( c , in.Library )
	if err != nil { return }
	reply , err = handlers.webdavMe( security.UserFrom( c ) , account )
	return
}

type webdavNameRequest struct {
	libraryQuery
	Name string `json:"name" jsonschema:"minLength=2,maxLength=32" jsonschema_description:"2-32 characters: lowercase letters, digits and -, starting with a letter or digit." error:"name must be 2-32 lowercase letters, digits and -"`
}

// setWebDAVName chooses your WebDAV name, or changes it: the primary
// library's, or the named library's. Your files stay
// where they are: they are kept by account, not by name. The old address
// stops working at once.
//
// @route("POST /api/webdav/name", summary="Choose or change your WebDAV name",
//     tag="WebDAV", feature="webdav", errors=(403, 409),
//     description="The name is the folder in your WebDAV address. Changing it moves no file, and the old "
//         "address stops working at once, so every client needs the new one. 403 when an admin has "
//         "locked your name (account.name_locked).",
// )
func ( handlers *Handlers ) setWebDAVName( c fiber.Ctx , in *webdavNameRequest ) ( reply *webdavMeReply , err error ) {
	user := security.UserFrom( c )
	name := strings.ToLower( strings.TrimSpace( in.Name ) )
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	account , created , claim_err := models.RenameDAVLibrary( handlers.Store , user.ID , library_id , name , handlers.cfg().WebDAV.EnabledByDefault , false )
	switch {
	case errors.Is( claim_err , models.ErrDAVNameLocked ):
		err = refuse( fiber.StatusForbidden , claim_err.Error() )
		return
	case errors.Is( claim_err , models.ErrDAVNameTaken ):
		err = refuse( fiber.StatusConflict , claim_err.Error() )
		return
	case errors.Is( claim_err , models.ErrDAVNameInvalid ) , errors.Is( claim_err , models.ErrDAVNameReserved ):
		err = refuse( fiber.StatusBadRequest , claim_err.Error() )
		return
	case claim_err != nil:
		err = claim_err
		return
	}
	if root_err := handlers.DAV.EnsureRoot( account.ID ); root_err != nil {
		err = serverError( c , root_err )
		return
	}
	message := "webdav name changed"
	if created { message = "webdav name chosen" }
	handlers.audit( c , logs.KindAdmin , message , fiber.Map{ "name": name , "library_id": account.ID } )
	reply , err = handlers.webdavMe( user , account )
	return
}

type createLibraryRequest struct {
	Name  string `json:"name" jsonschema:"minLength=2,maxLength=32" jsonschema_description:"Its WebDAV name: 2-32 lowercase letters, digits and -. It is also the user name its app passwords go with." error:"name must be 2-32 lowercase letters, digits and -"`
	Label string `json:"label,omitempty" jsonschema:"maxLength=60" jsonschema_description:"What it is for: Work, Personal." error:"label must be at most 60 characters"`
}

// davLibraryRefusal answers each way making or changing a library can fail.
func davLibraryRefusal( cause error ) ( err error ) {
	switch {
	case errors.Is( cause , db.ErrNotFound ):
		err = refuse( fiber.StatusNotFound , "no such WebDAV library" )
	case errors.Is( cause , models.ErrDAVNameTaken ) , errors.Is( cause , models.ErrDAVLibraryLimit ):
		err = refuse( fiber.StatusConflict , cause.Error() )
	case errors.Is( cause , models.ErrDAVPrimary ) , errors.Is( cause , models.ErrDAVNameLocked ):
		err = refuse( fiber.StatusForbidden , cause.Error() )
	case errors.Is( cause , models.ErrDAVNameInvalid ) , errors.Is( cause , models.ErrDAVNameReserved ) , errors.Is( cause , models.ErrDAVLabel ):
		err = refuse( fiber.StatusBadRequest , cause.Error() )
	default:
		err = cause
	}
	return
}

type webdavLibrariesReply struct {
	Libraries    []webdavLibraryRow `json:"libraries"`
	MaxLibraries int                `json:"max_libraries" jsonschema_description:"How many you may have; 0 is no limit."`
}

// listWebDAVLibraries is your WebDAV libraries.
//
// @route("GET /api/webdav/libraries", summary="Your WebDAV libraries",
//     tag="WebDAV", feature="webdav",
//     description="Each library is its own WebDAV folder, with its own name, app passwords, quota and "
//         "metadata library (say one per Zotero profile). Every WebDAV and library route takes "
//         "library=<name> to pick one; without it they use the first.",
// )
func ( handlers *Handlers ) listWebDAVLibraries( c fiber.Ctx , in *apidoc.None ) ( reply *webdavLibrariesReply , err error ) {
	owned , err := models.ListDAVLibrariesForOwner( handlers.Store , security.UserFrom( c ).ID )
	if err != nil { return }
	reply = &webdavLibrariesReply{ Libraries: []webdavLibraryRow{} , MaxLibraries: handlers.cfg().WebDAV.MaxLibraries }
	for _ , library := range owned {
		reply.Libraries = append( reply.Libraries , handlers.webdavLibraryRow( library ) )
	}
	return
}

// createWebDAVLibrary makes another library. The first one you make is your
// primary, the same as choosing your WebDAV name.
//
// @route("POST /api/webdav/libraries", summary="Make a WebDAV library",
//     tag="WebDAV", feature="webdav", status=201, errors=(409,),
//     description="A new, empty WebDAV folder with its own name, app passwords and quota. 409 when the "
//         "name is taken or you already have webdav.max_libraries_per_user.",
// )
func ( handlers *Handlers ) createWebDAVLibrary( c fiber.Ctx , in *createLibraryRequest ) ( reply *webdavLibraryRow , err error ) {
	cfg := handlers.cfg()
	user := security.UserFrom( c )
	account , create_err := models.CreateDAVLibrary( handlers.Store , user.ID , strings.ToLower( strings.TrimSpace( in.Name ) ) , in.Label , cfg.WebDAV.EnabledByDefault , cfg.WebDAV.MaxLibraries )
	if create_err != nil {
		err = davLibraryRefusal( create_err )
		return
	}
	if root_err := handlers.DAV.EnsureRoot( account.ID ); root_err != nil {
		err = serverError( c , root_err )
		return
	}
	row := handlers.webdavLibraryRow( account )
	reply = &row
	handlers.audit( c , logs.KindAdmin , "webdav library created" , fiber.Map{ "library_id": account.ID , "name": account.Name } )
	return
}

type libraryIDPath struct {
	LibraryID uint64 `json:"library_id" path:"library_id" jsonschema:"minimum=1" jsonschema_description:"The library's id, from GET /api/webdav/libraries." error:"invalid library id"`
}

type labelLibraryRequest struct {
	libraryIDPath
	Label string `json:"label" jsonschema:"maxLength=60" jsonschema_description:"What it is for. Empty clears it." error:"label must be at most 60 characters"`
}

// labelWebDAVLibrary changes one of your libraries' label.
//
// @route("POST /api/webdav/libraries/{library_id}/label", summary="Label a WebDAV library",
//     tag="WebDAV", feature="webdav", errors=(404,),
// )
func ( handlers *Handlers ) labelWebDAVLibrary( c fiber.Ctx , in *labelLibraryRequest ) ( reply *webdavLibraryRow , err error ) {
	account , label_err := models.SetDAVLabel( handlers.Store , security.UserFrom( c ).ID , in.LibraryID , in.Label )
	if label_err != nil {
		err = davLibraryRefusal( label_err )
		return
	}
	row := handlers.webdavLibraryRow( account )
	reply = &row
	return
}

// deleteWebDAVLibrary removes one of your libraries, not the first.
//
// @route("POST /api/webdav/libraries/{library_id}/delete", summary="Delete a WebDAV library",
//     tag="WebDAV", feature="webdav", errors=(403, 404),
//     description="Its app passwords stop at once, its metadata goes, and its files are removed by the "
//         "next WebDAV upkeep run. There is no undo. Your first library cannot be deleted (403).",
// )
func ( handlers *Handlers ) deleteWebDAVLibrary( c fiber.Ctx , in *libraryIDPath ) ( reply *okReply , err error ) {
	account , delete_err := models.DeleteDAVLibrary( handlers.Store , security.UserFrom( c ).ID , in.LibraryID )
	if delete_err != nil {
		err = davLibraryRefusal( delete_err )
		return
	}
	reply = &okReply{ Ok: true }
	handlers.audit( c , logs.KindAdmin , "webdav library deleted" , fiber.Map{ "library_id": account.ID , "name": account.Name } )
	return
}

// ---------------------------------------------------------------------------
// app passwords
// ---------------------------------------------------------------------------

type davPasswordRow struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	LibraryID  uint64    `json:"library_id" jsonschema_description:"The one WebDAV library it opens."`
	Library    string    `json:"library,omitempty" jsonschema_description:"That library's WebDAV name: the user name that goes with it."`
	CreatedAt  time.Time `json:"created_at"`
	Live       bool      `json:"live"`
	Revoked    bool      `json:"revoked"`
	Expired    bool      `json:"expired"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
}

func ( davPasswordRow ) APIName() ( name string ) { name = "AppPassword"; return }

func newDAVPasswordRow( password *models.DAVPassword , names map[uint64]string ) ( row davPasswordRow ) {
	row = davPasswordRow{ ID: password.ID , Name: password.Name , LibraryID: password.LibraryID , Library: names[ password.LibraryID ] , CreatedAt: password.CreatedAt , Live: password.Live() , Revoked: password.Revoked() , Expired: password.Expired() }
	if password.ExpiresAt != nil { row.ExpiresAt = *password.ExpiresAt }
	if password.LastUsedAt != nil { row.LastUsedAt = *password.LastUsedAt }
	if password.RevokedAt != nil { row.RevokedAt = *password.RevokedAt }
	return
}

type davPasswordsReply struct {
	Passwords []davPasswordRow `json:"passwords"`
	Paged
}

type listDAVPasswordsRequest struct {
	libraryQuery
	All string `json:"all,omitempty" query:"all" jsonschema:"enum=true" jsonschema_description:"true: every library's app passwords, not just library's."`
	Page
}

// libraryNames maps the caller's library ids to their WebDAV names.
func ( handlers *Handlers ) libraryNames( user_id uint64 ) ( names map[uint64]string ) {
	names = map[uint64]string{}
	if owned , err := models.ListDAVLibrariesForOwner( handlers.Store , user_id ); err == nil {
		for _ , library := range owned { names[ library.ID ] = library.Name }
	}
	return
}

// listDAVPasswords is one library's app passwords (library, default your
// first), or with all=true every library's, dead ones included and marked.
//
// @route("GET /api/webdav/passwords", summary="Your app passwords",
//     tag="WebDAV", access=SESSION_ONLY, feature="webdav", errors=(404,),
// )
func ( handlers *Handlers ) listDAVPasswords( c fiber.Ctx , in *listDAVPasswordsRequest ) ( reply *davPasswordsReply , err error ) {
	user := security.UserFrom( c )
	only := uint64( 0 )
	if in.All != "true" {
		if only , err = handlers.libraryOf( c , in.Library ); err != nil { return }
	}
	passwords , err := models.ListDAVPasswordsForUser( handlers.Store , user.ID )
	if err != nil { return }
	names := handlers.libraryNames( user.ID )
	rows := []davPasswordRow{}
	for _ , password := range passwords {
		if only != 0 && password.LibraryID != only { continue }
		rows = append( rows , newDAVPasswordRow( password , names ) )
	}
	reply = &davPasswordsReply{}
	reply.Passwords , reply.Paged = cutPage( rows , in.Page )
	return
}

type createDAVPasswordRequest struct {
	libraryQuery
	Name          string `json:"name" jsonschema:"minLength=1,maxLength=80" jsonschema_description:"What uses it: Zotero on my laptop, PRMA." error:"name must be 1-80 characters"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty" jsonschema:"minimum=0,maximum=3650" jsonschema_description:"Absent, null or 0: never expires." error:"expires_in_days must be 0 (never) or up to 3650"`
}

type newDAVPasswordReply struct {
	davPasswordRow
	Credential string `json:"credential" jsonschema_description:"The password. Shown in this response only, and never recoverable."`
	Username   string `json:"username" jsonschema_description:"The library's WebDAV name: the user name that goes with it."`
}

func ( newDAVPasswordReply ) APIName() ( name string ) { name = "NewAppPassword"; return }

// createDAVPassword makes an app password and shows it once.
//
// @route("POST /api/webdav/passwords", summary="Make an app password",
//     tag="WebDAV", access=SESSION_ONLY, feature="webdav", status=201, errors=(409,),
//     description="An app password signs a WebDAV client in to one of your libraries (library, default your "
//         "first), with that library's WebDAV name as the user name. It reaches that library only, and "
//         "never this API. It is shown in this response only.",
// )
func ( handlers *Handlers ) createDAVPassword( c fiber.Ctx , in *createDAVPasswordRequest ) ( reply *newDAVPasswordReply , err error ) {
	name := strings.TrimSpace( in.Name )
	if name == "" || len( name ) > 80 {
		err = refuse( fiber.StatusBadRequest , "name must be 1-80 characters" )
		return
	}
	ttl := time.Duration( 0 )
	if in.ExpiresInDays != nil { ttl = time.Duration( *in.ExpiresInDays ) * 24 * time.Hour }
	user := security.UserFrom( c )
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	credential , password , issue_err := models.IssueDAVPassword( handlers.Store , user.ID , library_id , name , ttl )
	switch {
	case errors.Is( issue_err , models.ErrDAVPasswordNoFolder ):
		err = refuse( fiber.StatusConflict , issue_err.Error() )
		return
	case errors.Is( issue_err , models.ErrDAVPasswordLimit ):
		err = refuse( fiber.StatusConflict , issue_err.Error() )
		return
	case errors.Is( issue_err , models.ErrAPIKeyDisabled ):
		err = refuse( fiber.StatusForbidden , "this account is disabled" )
		return
	case issue_err != nil:
		err = issue_err
		return
	}
	account , err := models.GetDAVAccount( handlers.Store , library_id )
	if err != nil { return }
	reply = &newDAVPasswordReply{ davPasswordRow: newDAVPasswordRow( password , map[uint64]string{ account.ID: account.Name } ) , Credential: credential , Username: account.Name }
	handlers.audit( c , logs.KindAdmin , "app password created" , fiber.Map{ "password_id": password.ID , "name": password.Name , "library_id": library_id } )
	return
}

type davPasswordPath struct {
	PasswordID string `json:"password_id" path:"password_id" jsonschema:"minLength=1,maxLength=64" jsonschema_description:"The app password's id, from GET /api/webdav/passwords." error:"invalid password id"`
}

func ( handlers *Handlers ) changeDAVPassword( c fiber.Ctx , in *davPasswordPath , message string , change func( *models.DAVPassword ) ) ( reply *davPasswordRow , err error ) {
	user := security.UserFrom( c )
	var password *models.DAVPassword
	var change_err error
	switch message {
	case "app password revoked":
		password , change_err = models.RevokeDAVPassword( handlers.Store , in.PasswordID , user.ID )
	case "app password deleted":
		password , change_err = models.DeleteDAVPassword( handlers.Store , in.PasswordID , user.ID )
	default:
		password , change_err = models.RestoreDAVPassword( handlers.Store , in.PasswordID , user.ID )
	}
	switch {
	case errors.Is( change_err , db.ErrNotFound ):
		err = refuse( fiber.StatusNotFound , "no such app password" )
		return
	case change_err != nil && message == "app password restored":
		err = restoreRefusal( change_err )
		return
	case change_err != nil:
		err = change_err
		return
	}
	row := newDAVPasswordRow( password , handlers.libraryNames( user.ID ) )
	reply = &row
	handlers.audit( c , logs.KindAdmin , message , fiber.Map{ "password_id": password.ID } )
	return
}

// revokeDAVPassword stops one of your app passwords for good.
//
// @route("POST /api/webdav/passwords/{password_id}/revoke", summary="Revoke an app password",
//     tag="WebDAV", access=SESSION_ONLY, feature="webdav", errors=(404,),
// )
func ( handlers *Handlers ) revokeDAVPassword( c fiber.Ctx , in *davPasswordPath ) ( reply *davPasswordRow , err error ) {
	reply , err = handlers.changeDAVPassword( c , in , "app password revoked" , nil )
	return
}

// deleteDAVPassword takes one of your app passwords away. It stops at once;
// restore brings it back for ten minutes.
//
// @route("POST /api/webdav/passwords/{password_id}/delete", summary="Delete an app password",
//     tag="WebDAV", access=SESSION_ONLY, feature="webdav", errors=(404,),
// )
func ( handlers *Handlers ) deleteDAVPassword( c fiber.Ctx , in *davPasswordPath ) ( reply *davPasswordRow , err error ) {
	reply , err = handlers.changeDAVPassword( c , in , "app password deleted" , nil )
	return
}

// restoreDAVPassword undoes a delete, within ten minutes.
//
// @route("POST /api/webdav/passwords/{password_id}/restore", summary="Put back a deleted app password",
//     tag="WebDAV", access=SESSION_ONLY, feature="webdav", errors=(404, 409),
// )
func ( handlers *Handlers ) restoreDAVPassword( c fiber.Ctx , in *davPasswordPath ) ( reply *davPasswordRow , err error ) {
	reply , err = handlers.changeDAVPassword( c , in , "app password restored" , nil )
	return
}

// ---------------------------------------------------------------------------
// files
// ---------------------------------------------------------------------------

// davFolder is one of the caller's libraries (name, or the primary),
// refusing a primary with no WebDAV name yet.
func ( handlers *Handlers ) davFolder( c fiber.Ctx , name string ) ( library_id uint64 , err error ) {
	account , err := handlers.libraryAccount( c , name )
	if err != nil { return }
	if account == nil {
		err = refuse( fiber.StatusConflict , "choose a WebDAV name first" )
		return
	}
	library_id = account.ID
	return
}

// fileRefusal answers each way a path can be refused.
func fileRefusal( cause error ) ( err error ) {
	switch {
	case errors.Is( cause , os.ErrNotExist ) , errors.Is( cause , dav.ErrTrashGone ):
		err = refuse( fiber.StatusNotFound , "no such file or folder" )
	case errors.Is( cause , dav.ErrNotFolder ):
		err = refuse( fiber.StatusBadRequest , "that is a file, not a folder" )
	case errors.Is( cause , dav.ErrNotFile ):
		err = refuse( fiber.StatusBadRequest , "that is a folder, not a file" )
	case errors.Is( cause , dav.ErrIsRoot ):
		err = refuse( fiber.StatusBadRequest , cause.Error() )
	case errors.Is( cause , dav.ErrRestoreTaken ):
		err = refuse( fiber.StatusConflict , cause.Error() )
	default:
		err = cause
	}
	return
}

type webdavPathQuery struct {
	libraryQuery
	Path string `json:"path,omitempty" query:"path" jsonschema:"maxLength=4096" jsonschema_description:"A path in your folder, like /zotero. Default /." error:"path must be at most 4096 characters"`
}

type listWebDAVFilesRequest struct {
	webdavPathQuery
	Page
}

type webdavEntryRow struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func ( webdavEntryRow ) APIName() ( name string ) { name = "WebDAVEntry"; return }

type webdavFilesReply struct {
	Path    string           `json:"path"`
	Entries []webdavEntryRow `json:"entries"`
	Paged
}

// listWebDAVFiles is one folder of your WebDAV folder.
//
// @route("GET /api/webdav/files", summary="List a folder",
//     tag="WebDAV", feature="webdav", errors=(404, 409),
//     description="What one folder of your WebDAV folder holds, folders first. Encrypted files show their "
//         "real size.",
// )
func ( handlers *Handlers ) listWebDAVFiles( c fiber.Ctx , in *listWebDAVFilesRequest ) ( reply *webdavFilesReply , err error ) {
	user_id , err := handlers.davFolder( c , in.Library )
	if err != nil { return }
	entries , list_err := handlers.DAV.List( user_id , in.Path )
	if list_err != nil {
		err = fileRefusal( list_err )
		return
	}
	rows := make( []webdavEntryRow , 0 , len( entries ) )
	for _ , entry := range entries {
		rows = append( rows , webdavEntryRow( entry ) )
	}
	reply = &webdavFilesReply{ Path: path.Clean( "/" + in.Path ) }
	reply.Entries , reply.Paged = cutPage( rows , in.Page )
	return
}

type downloadWebDAVRequest struct {
	libraryQuery
	Path string `json:"path" query:"path" jsonschema:"minLength=1,maxLength=4096" jsonschema_description:"The file, like /papers/smith2020.pdf." error:"path is required"`
}

// downloadWebDAVFile hands out one of your files, decrypted. It is always an
// attachment, never shown in the page: a file someone uploaded must not run
// as part of this site.
//
// @route("GET /api/webdav/download", summary="Download a file",
//     tag="WebDAV", feature="webdav", errors=(404, 409), reply_type="application/octet-stream",
// )
func ( handlers *Handlers ) downloadWebDAVFile( c fiber.Ctx , in *downloadWebDAVRequest ) ( err error ) {
	user_id , err := handlers.davFolder( c , in.Library )
	if err != nil { return }
	file , info , open_err := handlers.DAV.Open( user_id , in.Path )
	if open_err != nil {
		err = fileRefusal( open_err )
		return
	}
	err = sendDownload( c , file , file.Size() , info.Name() , info.ModTime() )
	return
}

// sendDownload streams a file as an attachment, closing it when done.
func sendDownload( c fiber.Ctx , reader io.ReadCloser , size int64 , name string , modified time.Time ) ( err error ) {
	c.Set( fiber.HeaderContentType , "application/octet-stream" )
	c.Set( fiber.HeaderContentDisposition , contentDisposition( name ) )
	c.Set( "Content-Security-Policy" , "sandbox; default-src 'none'" )
	c.Set( "X-Content-Type-Options" , "nosniff" )
	if modified.IsZero() == false { c.Set( fiber.HeaderLastModified , modified.UTC().Format( time.RFC1123 ) ) }
	// SendStream closes the reader once the body is written.
	err = c.SendStream( reader , int( size ) )
	return
}

// contentDisposition names a download, ASCII and UTF-8 both.
func contentDisposition( name string ) ( header string ) {
	fallback := strings.Map( func( r rune ) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '/' { return '_' }
		return r
	} , name )
	header = `attachment; filename="` + fallback + `"; filename*=UTF-8''` + pathEscape( name )
	return
}

func pathEscape( value string ) ( result string ) {
	var built strings.Builder
	for _ , b := range []byte( value ) {
		if ( b >= 'A' && b <= 'Z' ) || ( b >= 'a' && b <= 'z' ) || ( b >= '0' && b <= '9' ) || strings.IndexByte( "-._~" , b ) >= 0 {
			built.WriteByte( b )
			continue
		}
		built.WriteString( "%" + strings.ToUpper( string( "0123456789abcdef"[ b >> 4 ] ) + string( "0123456789abcdef"[ b & 15 ] ) ) )
	}
	result = built.String()
	return
}

type trashWebDAVRequest struct {
	libraryQuery
	Path string `json:"path" jsonschema:"minLength=1,maxLength=4096" jsonschema_description:"The file or folder to move to your trash." error:"path is required"`
}

type webdavTrashRow struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	DeletedAt time.Time `json:"deleted_at"`
}

func ( webdavTrashRow ) APIName() ( name string ) { name = "WebDAVTrashEntry"; return }

// trashWebDAVFile moves a file or folder to your trash, where it stays for
// webdav.trash_days and can be put back. It still counts against your quota
// until then.
//
// @route("POST /api/webdav/trash", summary="Move a file or folder to your trash",
//     tag="WebDAV", feature="webdav", errors=(404, 409),
//     description="It can be put back (POST /api/webdav/trash/{trash_id}/restore) until it is emptied, "
//         "after webdav.trash_days. It counts against your quota until then. A WebDAV client's own "
//         "deletes do not go through the trash.",
// )
func ( handlers *Handlers ) trashWebDAVFile( c fiber.Ctx , in *trashWebDAVRequest ) ( reply *webdavTrashRow , err error ) {
	user_id , err := handlers.davFolder( c , in.Library )
	if err != nil { return }
	entry , trash_err := handlers.DAV.Trash( user_id , in.Path )
	if trash_err != nil {
		err = fileRefusal( trash_err )
		return
	}
	row := webdavTrashRow( entry )
	reply = &row
	handlers.audit( c , logs.KindAdmin , "webdav file trashed" , fiber.Map{ "path": logs.Truncate( entry.Path , 300 ) } )
	return
}

type webdavTrashReply struct {
	Entries []webdavTrashRow `json:"entries"`
	Paged
}

type listWebDAVTrashRequest struct {
	libraryQuery
	Page
}

// listWebDAVTrash is your trash, newest first.
//
// @route("GET /api/webdav/trash", summary="Your trash",
//     tag="WebDAV", feature="webdav", errors=(409,),
// )
func ( handlers *Handlers ) listWebDAVTrash( c fiber.Ctx , in *listWebDAVTrashRequest ) ( reply *webdavTrashReply , err error ) {
	user_id , err := handlers.davFolder( c , in.Library )
	if err != nil { return }
	entries , err := handlers.DAV.ListTrash( user_id )
	if err != nil { return }
	rows := make( []webdavTrashRow , 0 , len( entries ) )
	for _ , entry := range entries {
		rows = append( rows , webdavTrashRow( entry ) )
	}
	reply = &webdavTrashReply{}
	reply.Entries , reply.Paged = cutPage( rows , in.Page )
	return
}

type webdavTrashPath struct {
	libraryQuery
	TrashID string `json:"trash_id" path:"trash_id" jsonschema:"minLength=16,maxLength=64" jsonschema_description:"The trash entry's id, from GET /api/webdav/trash." error:"invalid trash id"`
}

// restoreWebDAVFile puts a trash entry back where it was.
//
// @route("POST /api/webdav/trash/{trash_id}/restore", summary="Put a file or folder back",
//     tag="WebDAV", feature="webdav", errors=(404, 409),
// )
func ( handlers *Handlers ) restoreWebDAVFile( c fiber.Ctx , in *webdavTrashPath ) ( reply *webdavTrashRow , err error ) {
	user_id , err := handlers.davFolder( c , in.Library )
	if err != nil { return }
	entry , restore_err := handlers.DAV.Restore( user_id , in.TrashID )
	if restore_err != nil {
		err = fileRefusal( restore_err )
		return
	}
	row := webdavTrashRow( entry )
	reply = &row
	handlers.audit( c , logs.KindAdmin , "webdav file restored" , fiber.Map{ "path": logs.Truncate( entry.Path , 300 ) } )
	return
}

// emptyWebDAVTrashEntry removes one trash entry for good.
//
// @route("POST /api/webdav/trash/{trash_id}/delete", summary="Delete a trash entry for good",
//     tag="WebDAV", feature="webdav", errors=(404, 409),
// )
func ( handlers *Handlers ) emptyWebDAVTrashEntry( c fiber.Ctx , in *webdavTrashPath ) ( reply *okReply , err error ) {
	user_id , err := handlers.davFolder( c , in.Library )
	if err != nil { return }
	if empty_err := handlers.DAV.EmptyTrashEntry( user_id , in.TrashID ); empty_err != nil {
		err = fileRefusal( empty_err )
		return
	}
	reply = &okReply{ Ok: true }
	handlers.audit( c , logs.KindAdmin , "webdav trash entry deleted" , fiber.Map{ "trash_id": in.TrashID } )
	return
}

// ---------------------------------------------------------------------------
// admin
// ---------------------------------------------------------------------------

type adminWebDAVRow struct {
	UserID      uint64 `json:"user_id" jsonschema_description:"The account that owns the library."`
	DisplayName string `json:"display_name"`
	webdavAccountRow
}

func ( adminWebDAVRow ) APIName() ( name string ) { name = "AdminWebDAVAccount"; return }

type adminWebDAVReply struct {
	Accounts       []adminWebDAVRow `json:"accounts"`
	DefaultQuota   int64            `json:"default_quota_bytes"`
	Paged
}

type listAdminWebDAVRequest struct {
	Page
	Q    string `json:"q,omitempty" query:"q" jsonschema_description:"Keep accounts whose name or WebDAV name contains this, in any case."`
	Sort string `json:"sort,omitempty" query:"sort" jsonschema:"enum=display_name,enum=name,enum=used_bytes,enum=quota_bytes,enum=enabled" jsonschema_description:"Field to sort the whole list by."`
	Direction
}

// listAdminWebDAV is every WebDAV library: its owner, name, whether it may
// be used, its quota and how full it is. Never what is in it.
//
// @route("GET /api/admin/webdav", summary="Everyone's WebDAV folders",
//     tag="WebDAV", role=ADMIN, feature="webdav",
//     description="Every WebDAV library, with the account that owns it: whether it may be used, its quota "
//         "and how much it holds. Never what is in the folders.",
// )
func ( handlers *Handlers ) listAdminWebDAV( c fiber.Ctx , in *listAdminWebDAVRequest ) ( reply *adminWebDAVReply , err error ) {
	accounts , err := models.ListDAVAccounts( handlers.Store )
	if err != nil { return }
	names , err := handlers.displayNames()
	if err != nil { return }
	rows := []adminWebDAVRow{}
	for _ , account := range accounts {
		rows = append( rows , adminWebDAVRow{ UserID: account.UserID , DisplayName: names[ account.UserID ] , webdavAccountRow: *handlers.webdavAccountRow( account ) } )
	}
	rows = matchRows( rows , in.Q , "display_name" , "name" )
	reply = &adminWebDAVReply{ DefaultQuota: handlers.cfg().WebDAV.DefaultQuotaBytes }
	reply.Accounts , reply.Paged = sortedPage( rows , in.Sort , in.Direction , in.Page )
	return
}

type setAdminWebDAVRequest struct {
	libraryIDPath
	Enabled  *bool `json:"enabled,omitempty" jsonschema_description:"Whether the account may use WebDAV. Absent leaves it."`
	NameLocked *bool `json:"name_locked,omitempty" jsonschema_description:"true stops the account changing its own WebDAV name. Absent leaves it."`
	QuotaGB  *int  `json:"quota_gb,omitempty" jsonschema:"minimum=0,maximum=1048576" jsonschema_description:"The account's own quota in GB, 0 for no limit. Absent leaves it." error:"quota_gb must be 0-1048576"`
	DefaultQuota bool `json:"default_quota,omitempty" jsonschema_description:"true puts the account back on webdav.default_quota_gb."`
}

// setAdminWebDAV switches a library on or off, locks or unlocks its name, or
// sets its quota. Who may do it to whom is the same ladder as every account
// action, against the library's owner.
//
// @route("POST /api/admin/webdav/{library_id}", summary="Set someone's WebDAV access or quota",
//     tag="WebDAV", role=ADMIN, feature="webdav", errors=(403, 404),
//     description="library_id is from GET /api/admin/webdav. An account's first library has the account's "
//         "own id, so the user id still works for it.",
// )
func ( handlers *Handlers ) setAdminWebDAV( c fiber.Ctx , in *setAdminWebDAVRequest ) ( reply *adminWebDAVRow , err error ) {
	var quota *int64
	if in.QuotaGB != nil {
		bytes := int64( *in.QuotaGB ) * 1024 * 1024 * 1024
		quota = &bytes
	}
	account , set_err := models.SetDAVAccess( handlers.Store , security.UserFrom( c ).ID , in.LibraryID , in.Enabled , in.NameLocked , quota , in.DefaultQuota )
	switch {
	case errors.Is( set_err , db.ErrNotFound ):
		err = refuse( fiber.StatusNotFound , "no such WebDAV library" )
		return
	case errors.Is( set_err , models.ErrOwnerProtected ) , errors.Is( set_err , models.ErrOwnerRequired ):
		err = refuse( fiber.StatusForbidden , set_err.Error() )
		return
	case set_err != nil:
		err = set_err
		return
	}
	names , err := handlers.displayNames()
	if err != nil { return }
	reply = &adminWebDAVRow{ UserID: account.UserID , DisplayName: names[ account.UserID ] , webdavAccountRow: *handlers.webdavAccountRow( account ) }
	handlers.audit( c , logs.KindAdmin , "webdav access changed" , fiber.Map{ "user_id": account.UserID , "library_id": account.ID , "enabled": account.Enabled , "name_locked": account.NameLocked , "quota_bytes": reply.QuotaBytes } )
	return
}

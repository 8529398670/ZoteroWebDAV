package routes

import (
	fiber "github.com/gofiber/fiber/v3"

	models "zoterowebdav/server/models"
)

type itemMove struct {
	From string         `json:"from" jsonschema:"minLength=8,maxLength=8" jsonschema_description:"The item's key now." error:"from must be a Zotero key: 8 of A-Z and 2-9"`
	To   string         `json:"to,omitempty" jsonschema:"maxLength=8" jsonschema_description:"Its new key. Absent or the same: only set fields." error:"to must be a Zotero key: 8 of A-Z and 2-9"`
	Set  map[string]any `json:"set,omitempty" jsonschema_description:"Fields of its Zotero JSON to set (dateAdded, dateModified, ...). key and parentItem are left alone."`
}

type moveItemsRequest struct {
	libraryQuery
	Items  []itemMove `json:"items" jsonschema:"minItems=1,maxItems=1000" error:"items must list 1-1000 moves"`
	DryRun bool       `json:"dry_run,omitempty" jsonschema_description:"Say what would happen and change nothing."`
}

type itemMoveResult struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Status   string `json:"status" jsonschema:"enum=moved,enum=set,enum=unchanged,enum=refused"`
	Error    string `json:"error,omitempty"`
	Children int    `json:"children" jsonschema_description:"Attachments and notes now pointing at the new key."`
	Files    bool   `json:"files" jsonschema_description:"Its .zip and .prop moved with it in your WebDAV folder (or would, on a dry run)."`
}

type moveItemsReply struct {
	Results []itemMoveResult `json:"results"`
	DryRun  bool             `json:"dry_run"`
}

// moveLibraryItems moves items to other keys in place, or sets fields on
// them.
//
// @route("POST /api/library/items/move", summary="Move items to other keys, or set their fields",
//     tag="Library", feature="webdav",
//     description="Up to 1000 moves, all in one transaction. Each takes an item to a new key, sets the fields "
//         "given, points its attachments and notes at it, and leaves a tombstone under the old key, so "
//         "GET /api/library/changes shows the old key going and the new one coming. An attachment's .zip and "
//         ".prop in your WebDAV folder move with it. A move whose old key is not a live item, or whose new key "
//         "is taken, is refused (no chains or swaps) and the rest still go. It mends a "
//         "library imported from an export, which makes keys up, to Zotero's own keys and dates without "
//         "importing it again.",
// )
func ( handlers *Handlers ) moveLibraryItems( c fiber.Ctx , in *moveItemsRequest ) ( reply *moveItemsReply , err error ) {
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	moves := make( []models.LibraryMove , len( in.Items ) )
	for index , item := range in.Items {
		to := item.To
		if to == "" { to = item.From }
		moves[ index ] = models.LibraryMove{ From: item.From , To: to , Set: item.Set }
	}
	results , err := models.MoveLibraryItems( handlers.Store , library_id , moves , in.DryRun )
	if err != nil { return }
	reply = &moveItemsReply{ Results: make( []itemMoveResult , len( results ) ) , DryRun: in.DryRun }
	for index , result := range results {
		row := itemMoveResult{ From: result.From , To: result.To , Status: result.Status , Error: result.Error , Children: result.Children }
		if result.Status == "moved" {
			if in.DryRun {
				if attachment , get_err := models.GetDAVAttachment( handlers.Store , library_id , result.From ); get_err == nil {
					row.Files = attachment.HasZip || attachment.HasProp
				}
			} else if handlers.DAV != nil {
				moved , move_err := handlers.DAV.MoveAttachment( library_id , result.From , result.To )
				row.Files = moved
				if move_err != nil { row.Error = "the item moved, its files did not: " + move_err.Error() }
			}
		}
		reply.Results[ index ] = row
	}
	return
}

type collectionMove struct {
	From string         `json:"from" jsonschema:"minLength=8,maxLength=8" jsonschema_description:"The collection's key now." error:"from must be a Zotero key: 8 of A-Z and 2-9"`
	To   string         `json:"to,omitempty" jsonschema:"maxLength=8" jsonschema_description:"Its new key. Absent or the same: only set fields." error:"to must be a Zotero key: 8 of A-Z and 2-9"`
	Set  map[string]any `json:"set,omitempty" jsonschema_description:"Fields of its Zotero JSON to set. key and parentCollection are left alone."`
}

type moveCollectionsRequest struct {
	libraryQuery
	Collections []collectionMove `json:"collections" jsonschema:"minItems=1,maxItems=1000" error:"collections must list 1-1000 moves"`
	DryRun      bool             `json:"dry_run,omitempty" jsonschema_description:"Say what would happen and change nothing."`
}

type collectionMoveResult struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Status   string `json:"status" jsonschema:"enum=moved,enum=set,enum=unchanged,enum=refused"`
	Error    string `json:"error,omitempty"`
	Children int    `json:"children" jsonschema_description:"Subcollections and items filed in it, now pointing at the new key."`
}

type moveCollectionsReply struct {
	Results []collectionMoveResult `json:"results"`
	DryRun  bool                   `json:"dry_run"`
}

// moveLibraryCollections moves collections to other keys in place.
//
// @route("POST /api/library/collections/move", summary="Move collections to other keys",
//     tag="Library", feature="webdav",
//     description="Up to 1000 moves, all in one transaction, as POST /api/library/items/move does for items. "
//         "Each takes a collection to a new key, points its subcollections at it, lists the new key in every "
//         "item filed in it, and leaves a tombstone under the old key. A move whose old key is not a live "
//         "collection, or whose new key is taken, is refused and the rest still go.",
// )
func ( handlers *Handlers ) moveLibraryCollections( c fiber.Ctx , in *moveCollectionsRequest ) ( reply *moveCollectionsReply , err error ) {
	library_id , err := handlers.libraryOf( c , in.Library )
	if err != nil { return }
	moves := make( []models.LibraryMove , len( in.Collections ) )
	for index , collection := range in.Collections {
		to := collection.To
		if to == "" { to = collection.From }
		moves[ index ] = models.LibraryMove{ From: collection.From , To: to , Set: collection.Set }
	}
	results , err := models.MoveLibraryCollections( handlers.Store , library_id , moves , in.DryRun )
	if err != nil { return }
	reply = &moveCollectionsReply{ Results: make( []collectionMoveResult , len( results ) ) , DryRun: in.DryRun }
	for index , result := range results {
		reply.Results[ index ] = collectionMoveResult{ From: result.From , To: result.To , Status: result.Status , Error: result.Error , Children: result.Children }
	}
	return
}

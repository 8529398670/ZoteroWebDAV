package middleware

import (
	fiber "github.com/gofiber/fiber/v3"

	config "zoterowebdav/server/config"
)

// BodyLimit holds every request to server.body_limit_bytes except those to
// the paths in larger, which may send up to their own limit. Fiber's own
// BodyLimit is one number for the whole app, so main.go sets it to the
// largest of these and this puts the rest back.
func BodyLimit( settings *config.Holder , larger map[string]int ) ( handler fiber.Handler ) {
	handler = func( c fiber.Ctx ) ( err error ) {
		limit := settings.Get().BodyLimit
		if own , ok := larger[ c.Path() ]; ok && c.Method() == fiber.MethodPost { limit = max( limit , own ) }
		if c.Request().Header.ContentLength() > limit || len( c.Request().Body() ) > limit {
			err = c.Status( fiber.StatusRequestEntityTooLarge ).JSON( fiber.Map{ "error": "request body too large" } )
			return
		}
		err = c.Next()
		return
	}
	return
}

// Command buildjs compiles web/src/app.ts → internal/web/static/app.js using
// esbuild's Go API, so building the site's client JS needs no Node toolchain.
// esbuild transpiles but does NOT type-check; `make js` runs tsc --noEmit after
// this when available. Run from the repo root: go run ./tools/buildjs
package main

import (
	"fmt"
	"os"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	res := api.Build(api.BuildOptions{
		EntryPoints: []string{"web/src/app.ts"},
		Outfile:     "internal/web/static/app.js",
		Bundle:      true, // resolves relative imports; npm packages are deliberately unresolvable
		Format:      api.FormatIIFE,
		Target:      api.ES2020,
		Charset:     api.CharsetUTF8,
		Write:       true,
		LogLevel:    api.LogLevelInfo,
	})
	if len(res.Errors) > 0 {
		os.Exit(1)
	}
	fmt.Println("buildjs: web/src/app.ts → internal/web/static/app.js")
}

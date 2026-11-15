package webassets

import "embed"

// FS embeds the standalone bilingual frontend assets so triage-hub can run as a single self-contained binary.
//
//go:embed index.html i18n/*.js
var FS embed.FS

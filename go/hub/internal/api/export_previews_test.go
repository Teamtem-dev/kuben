package api

// PullStateOf exposes pullStateOf: `open`, `closed` or `unknown`.
func PullStateOf(open bool, err error) string { return string(pullStateOf(open, err)) }

// Preview internals under test.
var (
	PreviewDtoOf       = previewDto
	CheckPreviewPolicy = checkPreviewPolicy
	PreviewConfig      = previewConfig
	PullAudit          = pullAudit
)

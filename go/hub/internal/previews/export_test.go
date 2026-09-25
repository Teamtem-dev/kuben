package previews

// PullStateOf exposes pullStateOf: `open`, `closed` or `unknown`.
func PullStateOf(open bool, err error) string { return string(pullStateOf(open, err)) }

// Preview internals under test.
var (
	PreviewConfig = previewConfig
	PullAudit     = pullAudit
)

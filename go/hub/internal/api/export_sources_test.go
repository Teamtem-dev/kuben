package api

// Internals of the build, scan, source and Git routes under test.
var (
	BuildDtoOf     = buildDto
	BuildID        = buildID
	CheckException = checkException
	ExceptionDtoOf = exceptionDto
	ScanDtoOf      = scanDto
	AppScansDtoOf  = appScansDto
	SbomFile       = sbomFile
	ProviderError  = providerError
)

// GithubWebhookPath is where GitHub delivers webhooks.
const GithubWebhookPath = githubWebhookPath

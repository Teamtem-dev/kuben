package api

// Internals under test.
var (
	SpecFromCreate     = specFromCreate
	ApplyUpdate        = applyUpdate
	ValidateSpec       = validateSpec
	QuantityBytes      = quantityBytes
	ValidateVolumes    = validateVolumes
	CheckNoShrink      = checkNoShrink
	FromCRDEnv         = fromCRDEnv
	ToCRDEnv           = toCRDEnv
	ToCRDVolumes       = toCRDVolumes
	AppDtoOf           = appDto
	ConfigOf           = configOf
	DesiredSpec        = desiredSpec
	SpecOf             = specOf
	EnvLimits          = environmentLimits
	Peak               = peak
	PinnedImage        = pinnedImage
	IdempotencyKey     = idempotencyKey
	RollbackSpec       = rollbackSpec
	ReleaseReason      = releaseReason
	ToDomains          = toDomains
	PromoteSpec        = promoteSpec
	SpecChanges        = specChanges
	MissingSecrets     = missingSecrets
	ManualJobName      = manualJobName
	SplitLine          = splitLine
	Belongs            = belongs
	NewLogStreams      = newLogStreams
	RefusalErr         = refusalErr
	Eligible           = eligible
	PolicyOf           = policyOf
	PolicyDtoOf        = policyDto
	TemplatesCatalogue = templatesCatalogue
	RenderTemplate     = renderTemplate
	TemplateDtoOf      = templateDto
	StatusPageDtoOf    = statusPageDto
)

type (
	LogStreams      = logStreams
	LogStreamPermit = logStreamPermit
)

// AnyImage is the image admission measures configurations with.
const AnyImage = anyImage

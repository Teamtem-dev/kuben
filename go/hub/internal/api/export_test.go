package api

// Internals under test.
var (
	SpecFromCreate  = specFromCreate
	ApplyUpdate     = applyUpdate
	ValidateSpec    = validateSpec
	QuantityBytes   = quantityBytes
	ValidateVolumes = validateVolumes
	CheckNoShrink   = checkNoShrink
	FromCRDEnv      = fromCRDEnv
	ToCRDEnv        = toCRDEnv
	ToCRDVolumes    = toCRDVolumes
	AppDtoOf        = appDto
	ConfigOf        = configOf
	DesiredSpec     = desiredSpec
	SpecOf          = specOf
	EnvLimits       = environmentLimits
	Peak            = peak
	PinnedImage     = pinnedImage
	IdempotencyKey  = idempotencyKey
	RollbackSpec    = rollbackSpec
	ReleaseReason   = releaseReason
	ToDomains       = toDomains
)

// AnyImage is the image admission measures configurations with.
const AnyImage = anyImage

package api

// Doctor internals under test (routes/apps/doctor.rs).
var DoctorReportOf = doctorReport

// FactsFreshMs is how old recorded cluster facts may be.
const FactsFreshMs = factsFreshMs

// AgentStaleAfter is how long an agent may stay silent.
var AgentStaleAfter = agentStaleAfter

// Evidence internals under test (routes/apps/evidence.rs).
var (
	DeploymentNode = deploymentNode
	FromChecks     = fromChecks
	PodsNode       = podsNode
	DebugName      = debugName
)

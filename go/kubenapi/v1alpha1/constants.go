package v1alpha1

// Well-known labels (the Rust labels module).
const (
	// LabelManagedBy is the standard label naming the managing tool.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelManagerValue is the value of LabelManagedBy on what Kuben manages.
	LabelManagerValue = "kuben"
	// LabelOrg names the organization that owns a resource.
	LabelOrg = "kuben.dev/org"
	// LabelProject names the project a resource belongs to.
	LabelProject = "kuben.dev/project"
	// LabelEnvironment names the environment a resource belongs to.
	LabelEnvironment = "kuben.dev/environment"
	// LabelApp names the app a resource belongs to.
	LabelApp = "kuben.dev/app"
	// LabelProcess names the app process a workload runs.
	LabelProcess = "kuben.dev/process"
	// LabelGatewayOwner on a Gateway means Kuben may write its listeners.
	// Kuben sets it on the Gateway it creates; an operator sets it to
	// dedicate an existing one.
	LabelGatewayOwner = "kuben.dev/gateway-owner"
	// ManagedSelector is the label selector matching everything Kuben
	// manages.
	ManagedSelector = LabelManagedBy + "=" + LabelManagerValue
)

// Annotations the materializer writes on the objects it renders (the Rust
// materializer::render::annotations module).
const (
	// AnnotationGeneration is the target generation an App object was
	// written for: the generation fence.
	AnnotationGeneration = "kuben.dev/generation"
	// AnnotationOperation is the operation that wrote the object last.
	AnnotationOperation = "kuben.dev/operation"
	// AnnotationLifecycleUID is the target's lifecycle UID: a recreated
	// target is another App.
	AnnotationLifecycleUID = "kuben.dev/lifecycle-uid"
	// AnnotationID is the SQL id the object was rendered from: project,
	// environment or target.
	AnnotationID = "kuben.dev/id"
	// AnnotationHandover on an App handed over to the cluster's agent
	// (M1.9) holds the target id. The App controller leaves such an App
	// alone, so it never writes its workloads again while the App goes.
	AnnotationHandover = "kuben.dev/handover"
)

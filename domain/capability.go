// Package domain defines the core types of the computer-use automation system.
// It has zero external dependencies. All other packages depend on this one.
package domain

import "time"

// Capability is the versioned, serializable artifact emitted after a successful
// LLM planning run. It is the contract between the planner and the replay engine.
type Capability struct {
	ID           string       `json:"id"`
	Version      int          `json:"version"`
	Description  string       `json:"description"`
	Target       Target       `json:"target"`
	Params       []ParamDef   `json:"params"`
	Steps        []Step       `json:"steps"`
	SafetyPolicy SafetyPolicy `json:"safety_policy"`
	CreatedAt    time.Time    `json:"created_at"`
	CreatedBy    string       `json:"created_by"` // e.g. "llm:gpt-4o" or "human"
}

// Target describes the application under automation.
type Target struct {
	URL         string `json:"url"`
	Application string `json:"application"`
	Description string `json:"description"`
}

// ParamDef defines an input slot that can be substituted at replay time.
// This turns a recorded capability into a reusable, parameterized function.
type ParamDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"`    // "string" | "secret" | "number" | "bool"
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// SafetyPolicy is a human-defined hard ceiling that the engine enforces
// regardless of LLM output. It cannot be overridden by the planner.
type SafetyPolicy struct {
	MaxRiskAllowed    RiskLevel `json:"max_risk_allowed"`    // e.g. "medium"
	RequireApprovalAt RiskLevel `json:"require_approval_at"` // e.g. "high"
	AllowedDomains    []string  `json:"allowed_domains"`
	MaxMonetaryValue  *float64  `json:"max_monetary_value,omitempty"` // nil = no cap
}

// Step represents a single atomic interaction, assertion, or decision point.
// It is NOT a raw Playwright call — the engine translates it.
type Step struct {
	ID               string      `json:"id"`
	Type             StepType    `json:"type"`
	Description      string      `json:"description"`
	Locator          *Locator    `json:"locator,omitempty"`
	Action           *Action     `json:"action,omitempty"`
	Assert           *Assertion  `json:"assert,omitempty"`
	Branch           *Branch     `json:"branch,omitempty"`
	Risk             RiskLevel   `json:"risk"`
	RequiresApproval bool        `json:"requires_approval"`
	OnError          ErrorPolicy `json:"on_error"`
	TimeoutMs        int         `json:"timeout_ms"` // 0 = use system default
}

// StepType categorizes the step for the engine's dispatch logic.
type StepType string

const (
	StepTypeAction     StepType = "action"
	StepTypeAssert     StepType = "assert"
	StepTypeBranch     StepType = "branch"
	StepTypeExtract    StepType = "extract"
	StepTypeWait       StepType = "wait"
	StepTypeCheckpoint StepType = "checkpoint"
)

// RiskLevel classifies the potential impact of a step.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// Locator uses a priority-ordered fallback chain. The engine tries Primary first,
// then each Fallback in order. This survives minor UI changes without re-planning.
type Locator struct {
	Primary   LocatorStrategy   `json:"primary"`
	Fallbacks []LocatorStrategy `json:"fallbacks,omitempty"`
	Frame     string            `json:"frame,omitempty"` // switch into this iframe first
}

// LocatorStrategy is a single targeting approach.
type LocatorStrategy struct {
	Kind  LocatorKind `json:"kind"`
	Value string      `json:"value"`
	Name  string      `json:"name,omitempty"` // ARIA accessible name (for "role" kind)
}

// LocatorKind determines how the engine finds an element.
// Priority (most to least stable): test-id > role > label > text > css > xpath
type LocatorKind string

const (
	LocatorKindTestID LocatorKind = "test-id"
	LocatorKindRole   LocatorKind = "role"
	LocatorKindLabel  LocatorKind = "label"
	LocatorKindText   LocatorKind = "text"
	LocatorKindCSS    LocatorKind = "css"
	LocatorKindXPath  LocatorKind = "xpath"
)

// Action describes what the engine does to a located element.
// Value supports "{{param_name}}" interpolation.
type Action struct {
	Kind  ActionKind `json:"kind"`
	Value string     `json:"value,omitempty"`
	Key   string     `json:"key,omitempty"` // for key_press: "Enter", "Tab", etc.
}

// ActionKind is the type of browser interaction.
type ActionKind string

const (
	ActionKindClick    ActionKind = "click"
	ActionKindFill     ActionKind = "fill"     // clear + type
	ActionKindSelect   ActionKind = "select"
	ActionKindCheck    ActionKind = "check"
	ActionKindUncheck  ActionKind = "uncheck"
	ActionKindKeyPress ActionKind = "key_press"
	ActionKindScroll   ActionKind = "scroll"
	ActionKindHover    ActionKind = "hover"
	ActionKindNavigate ActionKind = "navigate" // no locator needed
	ActionKindUpload   ActionKind = "upload"
)

// Assertion verifies page state without side effects.
//
// IsBusinessOutcome is the core of the error taxonomy:
//   - true:  assertion FAILING means we reached an expected business state.
//     Engine sets RunStatus = BusinessError and stops cleanly. No retry.
//   - false: assertion FAILING is an unexpected technical failure.
//     Engine applies OnError policy (retry / escalate / fail_fast).
type Assertion struct {
	Kind              AssertionKind `json:"kind"`
	Expected          string        `json:"expected"` // supports "{{param_name}}"
	IsBusinessOutcome bool          `json:"is_business_outcome"`
	CaptureAs         string        `json:"capture_as,omitempty"` // variable name for extract
}

// AssertionKind determines how the assertion is evaluated.
type AssertionKind string

const (
	AssertionKindTextVisible    AssertionKind = "text_visible"
	AssertionKindTextNotVisible AssertionKind = "text_not_visible"
	AssertionKindURLContains    AssertionKind = "url_contains"
	AssertionKindElementExists  AssertionKind = "element_exists"
	AssertionKindElementHidden  AssertionKind = "element_hidden"
	AssertionKindValueEquals    AssertionKind = "value_equals"
	AssertionKindCustomScript   AssertionKind = "custom_script"
)

// Branch lets the plan handle multiple expected page states without re-invoking
// the LLM. It evaluates a condition and jumps to the appropriate step ID.
type Branch struct {
	Condition Assertion `json:"condition"`
	IfTrue    []string  `json:"if_true"`  // step IDs to jump to
	IfFalse   []string  `json:"if_false"` // step IDs to jump to
}

// ErrorPolicy defines how the engine responds to a step failure.
type ErrorPolicy struct {
	// Strategy: "fail_fast" | "retry" | "skip" | "human_escalate"
	Strategy          string `json:"strategy"`
	MaxRetries        int    `json:"max_retries,omitempty"`
	RetryDelayMs      int    `json:"retry_delay_ms,omitempty"`
	EscalationMessage string `json:"escalation_message,omitempty"`
}

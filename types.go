package main

// Shared YAML types used across both EventBridge Rules and Scheduler
// Schedules. Each type appears identically (or nearly so) on both AWS
// services' SDK shapes; declaring once here keeps rule.go / schedule.go
// from drifting on the user-facing field names.

// RetryPolicy is identical between EventBridge Rules and Scheduler Schedules.
type RetryPolicy struct {
	MaximumRetryAttempts     int32 `yaml:"maximumRetryAttempts"`
	MaximumEventAgeInSeconds int32 `yaml:"maximumEventAgeInSeconds"`
}

// DeadLetterConfig is identical between Rules and Schedules.
type DeadLetterConfig struct {
	Arn string `yaml:"arn"`
}

// SqsParameters carries the FIFO MessageGroupId for an SQS target. Used by
// both EventBridge Rules and Scheduler Schedules.
type SqsParameters struct {
	MessageGroupId string `yaml:"messageGroupId,omitempty"`
}

// CapacityProviderStrategyItem maps to ebtypes / schtypes
// CapacityProviderStrategyItem. Mutually exclusive with launchType in the
// AWS API; that constraint is enforced in validate.go.
type CapacityProviderStrategyItem struct {
	CapacityProvider string `yaml:"capacityProvider"`
	Base             int32  `yaml:"base,omitempty"`
	Weight           int32  `yaml:"weight,omitempty"`
}

// PlacementConstraint matches the AWS ECS placement-constraint shape
// (e.g. distinctInstance / memberOf with a Cluster Query Language
// expression). Same shape on Rule and Schedule SDKs.
type PlacementConstraint struct {
	Type       string `yaml:"type"` // distinctInstance | memberOf
	Expression string `yaml:"expression,omitempty"`
}

// PlacementStrategy matches the AWS ECS placement-strategy shape
// (random / spread / binpack with an optional field like
// "attribute:ecs.availability-zone").
type PlacementStrategy struct {
	Type  string `yaml:"type"` // random | spread | binpack
	Field string `yaml:"field,omitempty"`
}

// KeyValuePair holds a Name / Value pair, used for ECS RunTask Tags
// passed through to the launched task.
type KeyValuePair struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value,omitempty"`
}

// SageMakerPipelineParameters supplies pipeline parameters when invoking a
// SageMaker pipeline as a target. Same shape on Rules and Schedules.
type SageMakerPipelineParameters struct {
	PipelineParameterList []SageMakerPipelineParameter `yaml:"pipelineParameterList,omitempty"`
}

// SageMakerPipelineParameter is one (Name, Value) pair in
// SageMakerPipelineParameters.PipelineParameterList.
type SageMakerPipelineParameter struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

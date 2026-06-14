package analyzer

func AllRules() []Rule {
	return []Rule{
		&UnhandledErrorRule{},
		&InfiniteLoopRiskRule{},
		&DeepNestingRule{},
		&HardcodedCredentialRule{},
		&DeadCodeRule{},
		&MissingDelayRule{},
		&DuplicateActionRule{},
		&UnusedVariableRule{},
		&UninitializedVariableRule{},
		&SlowPatternRule{},
		&EmptyHandlerRule{},
		&ResourceLeakRule{},
		&SubflowNoErrorHandlerRule{},
		&GotoAntipatternRule{},
		&EmptyBranchRule{},
		&RedundantActionRule{},
		&FileOpNoErrorHandlerRule{},
		&MissingTimeoutRule{},
		&SensitiveDataExposureRule{},
		&ErrorSwallowRule{},
		&MissingRetryRule{},
		&WideLoopRule{},
		&SubflowMismatchRule{},
		&DeadDataRule{},
		&HardcodedFilePathRule{},
		&SqlInjectionRiskRule{},
	}
}

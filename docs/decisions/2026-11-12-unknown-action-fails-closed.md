# A misspelt policy action is refused, never allowed

- **Problem**: nothing checked a rule's `action`. The code that acts on a decision only looks for `deny` and `human_approval`, so `action: human_aproval` reached neither branch and the call was forwarded. Found while testing whole-file policy edits: an invalid file was accepted.
- **Decision**: the loader refuses a rule whose action is not `allow`, `deny` or `human_approval`, naming the file and the rule (`Load` and `LoadPolicies`, so start, hot reload and control-plane edits alike). The engine also answers `deny` for an unknown action in a policy set it was handed by other means.
- **Why**: fail closed is the contract; a typo must cost a refused file, not a silent allow. Refusing at load keeps the running set in place, which the watcher and the edit endpoints already rely on.
- **Where**: `config/config.go` (`ValidAction`, `validatePolicyActions`), `policy/engine.go` (`Evaluate`); tests `TestLoadRejectsUnknownPolicyAction`, `TestEvaluateUnknownActionFailsClosed`.

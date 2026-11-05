package helper

// Exported for envname_test.go, which lives in package helper_test because it
// imports config, and config imports this package.
var (
	CheckEnvName   = checkEnvName
	SecretSuffixes = secretSuffixes
)

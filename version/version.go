package version

var (
	CurrentCommit string

	Version = "v2.6.0"
)

func UserVersion() string {
	return Version + CurrentCommit
}

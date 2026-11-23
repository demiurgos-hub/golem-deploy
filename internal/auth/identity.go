package auth

// Identity kinds accepted by request-context helpers.
const (
	KindSession = "session"
	KindAPI     = "api"
)

// Identity is the authenticated principal stored on the request context.
type Identity struct {
	Username    string
	AccessLevel int
	Kind        string
}

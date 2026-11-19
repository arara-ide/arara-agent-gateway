package gateway

import (
	"os"
)

var apiSources = os.DirFS("internal/api")

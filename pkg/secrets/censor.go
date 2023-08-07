package secrets

import (
	"os"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/secretutil"
)

// DynamicCensor keeps a list of censored secrets that is dynamically updated.
// Used when the list of secrets to censor is updated during the execution of
// the program and cannot be determined in advance.  Access to the list of
// secrets is internally synchronized.
type DynamicCensor struct {
	sync.RWMutex
	*secretutil.ReloadingCensorer
	secrets sets.String
}

func NewDynamicCensor() DynamicCensor {
	return DynamicCensor{
		ReloadingCensorer: secretutil.NewCensorer(),
		secrets:           sets.NewString(),
	}
}

// AddSecrets adds the content of one or more secrets to the censor list.
func (c *DynamicCensor) AddSecrets(s ...string) {
	c.Lock()
	defer c.Unlock()
	c.secrets.Insert(s...)
	c.ReloadingCensorer.Refresh(c.secrets.List()...)
}

// ReadFromEnv loads an environment variable and adds it to the censor list.
func ReadFromEnv(name string, censor *DynamicCensor) string {
	ret := os.Getenv(name)
	if ret != "" {
		censor.AddSecrets(ret)
	}
	return ret
}

// ReadFromFile loads content from a file and adds it to the censor list.
func ReadFromFile(path string, censor *DynamicCensor) (string, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	ret := strings.TrimSpace(string(bytes))
	censor.AddSecrets(ret)
	return ret, nil
}

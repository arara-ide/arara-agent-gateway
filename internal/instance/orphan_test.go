package instance_test

import "github.com/arara-ide/arara-agent-gateway/internal/docker"

func dockerOrphan() docker.ContainerSpec {
	return docker.ContainerSpec{Name: "aag-inst_orphan", Labels: map[string]string{"aag.instance": "inst_orphan"}}
}

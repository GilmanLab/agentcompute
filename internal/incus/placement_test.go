package incus

import (
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
)

func TestPickLeastLoadedPrefersFreeRAMThenLoadThenName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		members []memberLoad
		want    string
	}{
		{
			name: "highest free RAM wins",
			members: []memberLoad{
				{name: "lab01", freeRAM: 1 << 30, load: 0.1},
				{name: "lab02", freeRAM: 4 << 30, load: 2.0},
				{name: "lab03", freeRAM: 2 << 30, load: 0.0},
			},
			want: "lab02",
		},
		{
			name: "equal RAM prefers lower load",
			members: []memberLoad{
				{name: "lab01", freeRAM: 2 << 30, load: 1.5},
				{name: "lab03", freeRAM: 2 << 30, load: 0.2},
			},
			want: "lab03",
		},
		{
			name: "equal RAM and load prefers name",
			members: []memberLoad{
				{name: "lab03", freeRAM: 2 << 30, load: 0.5},
				{name: "lab01", freeRAM: 2 << 30, load: 0.5},
			},
			want: "lab01",
		},
		{
			name:    "empty set",
			members: nil,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, pickLeastLoaded(tt.members))
		})
	}
}

func TestAutoPlaceableSkipsManualAndGroupSchedulers(t *testing.T) {
	t.Parallel()

	assert.True(t, autoPlaceable(clusterMemberConfig("")))
	assert.True(t, autoPlaceable(clusterMemberConfig("all")))
	assert.False(t, autoPlaceable(clusterMemberConfig("manual")))
	assert.False(t, autoPlaceable(clusterMemberConfig("group")))
}

func clusterMemberConfig(scheduler string) api.ClusterMember {
	return api.ClusterMember{
		ClusterMemberPut: api.ClusterMemberPut{
			Config: api.ConfigMap{"scheduler.instance": scheduler},
		},
	}
}

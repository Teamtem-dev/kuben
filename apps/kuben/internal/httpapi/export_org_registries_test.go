package httpapi

import (
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// RegistryServer is registryServer of the preset named preset.
func RegistryServer(preset, given string) (string, error) {
	p, _ := presetOf(store.RegistryPreset(preset))
	return registryServer(p, given)
}

// CheckNewRegistry is checkNewRegistry without its result.
func CheckNewRegistry(req *gen.CreateOrgRegistry) error {
	_, err := checkNewRegistry(req)
	return err
}

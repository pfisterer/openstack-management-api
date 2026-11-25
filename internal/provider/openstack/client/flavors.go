package osclient

import (
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/pagination"
)

// FlavorInfo is a flavour as the resource estimator sees it: its size, whether everybody may use it, and its GPUs.
type FlavorInfo struct {
	ID       string
	Name     string
	VCPUs    int
	RAMMB    int
	DiskGB   int
	GPUs     int
	IsPublic bool
}

// ListFlavors lists every flavour, public and private (the service user is an admin). The GPUs of a private flavour are read from its extra specs (resources:VGPU, pci_passthrough:alias); public flavours carry none here, so their specs are not asked for.
func (c *OpenStackClient) ListFlavors() ([]FlavorInfo, error) {
	var out []FlavorInfo
	err := flavors.ListDetail(c.computeSvc(), flavors.ListOpts{AccessType: flavors.AllAccess}).EachPage(func(page pagination.Page) (bool, error) {
		fs, err := flavors.ExtractFlavors(page)
		if err != nil {
			return false, err
		}
		for _, f := range fs {
			info := FlavorInfo{ID: f.ID, Name: f.Name, VCPUs: f.VCPUs, RAMMB: f.RAM, DiskGB: f.Disk, IsPublic: f.IsPublic}
			if !f.IsPublic {
				if specs, err := flavors.ListExtraSpecs(c.computeSvc(), f.ID).Extract(); err == nil {
					info.GPUs = GPUsFromExtraSpecs(specs)
				}
			}
			out = append(out, info)
		}
		return true, nil
	})
	return out, err
}

// GPUsFromExtraSpecs counts the GPUs a flavour's extra specs ask for: resources:VGPU=N (a vGPU), or pci_passthrough:alias=name:N[,name:M] (whole GPUs).
func GPUsFromExtraSpecs(specs map[string]string) int {
	if v, ok := specs["resources:VGPU"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	total := 0
	for _, part := range strings.Split(specs["pci_passthrough:alias"], ",") {
		_, count, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(count)); err == nil && n > 0 {
			total += n
		}
	}
	return total
}

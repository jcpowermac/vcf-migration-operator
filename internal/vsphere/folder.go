package vsphere

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/object"
	"k8s.io/klog/v2"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// CreateVMFolder creates a VM folder under the datacenter's vmFolder. The
// folderPath can be specified as a bare name (e.g. "my-folder"), a path
// relative to the vm inventory folder (e.g. "my-folder/sub"), or a full
// inventory path (e.g. "/{datacenter}/vm/my-folder"). The function returns
// the created folder.
func CreateVMFolder(ctx context.Context, s *Session, folderPath string) (*object.Folder, error) {
	if s == nil || s.Finder == nil {
		return nil, fmt.Errorf("session and Finder must not be nil")
	}
	log := klog.FromContext(ctx)

	folderName := parseFolderName(s.Datacenter, folderPath)
	log.V(2).Info("creating VM folder", "folder", folderName, "datacenter", s.Datacenter)

	dc, err := s.Finder.Datacenter(ctx, s.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("finding datacenter %q: %w", s.Datacenter, err)
	}

	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting datacenter folders: %w", err)
	}

	vmFolder := folders.VmFolder
	if vmFolder == nil {
		return nil, fmt.Errorf("datacenter %q has no VM folder", s.Datacenter)
	}

	parts := strings.Split(folderName, "/")
	parent := vmFolder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if err := execution.Check(ctx); err != nil {
			return nil, err
		}
		created, err := parent.CreateFolder(ctx, part)
		if err != nil {
			return nil, fmt.Errorf("creating folder %q: %w", part, err)
		}
		parent = created
	}

	// CreateFolder returns a reference without InventoryPath populated.
	// Look up the folder through the Finder to get the full path.
	result, err := GetVMFolder(ctx, s, folderName)
	if err != nil {
		return nil, fmt.Errorf("looking up created folder %q: %w", folderName, err)
	}

	log.V(2).Info("created VM folder", "path", result.InventoryPath)
	return result, nil
}

// GetVMFolder looks up an existing VM folder by path. The folderPath can be a
// bare name, a relative path, or a full inventory path. It returns the folder
// object if found.
func GetVMFolder(ctx context.Context, s *Session, folderPath string) (*object.Folder, error) {
	if s == nil || s.Finder == nil {
		return nil, fmt.Errorf("session and Finder must not be nil")
	}
	log := klog.FromContext(ctx)

	folderName := parseFolderName(s.Datacenter, folderPath)
	searchPath := fmt.Sprintf("/%s/vm/%s", s.Datacenter, folderName)
	log.V(2).Info("looking up VM folder", "path", searchPath)

	folder, err := s.Finder.Folder(ctx, searchPath)
	if err != nil {
		return nil, fmt.Errorf("finding folder %q: %w", searchPath, err)
	}

	return folder, nil
}

// DeleteVMFolder destroys the given VM folder and all of its children.
func DeleteVMFolder(ctx context.Context, s *Session, folder *object.Folder) error {
	if folder == nil {
		return fmt.Errorf("folder must not be nil")
	}
	log := klog.FromContext(ctx)
	log.V(2).Info("deleting VM folder", "path", folder.InventoryPath)

	if err := execution.Check(ctx); err != nil {
		return err
	}
	task, err := folder.Destroy(ctx)
	if err != nil {
		return fmt.Errorf("destroying folder %s: %w", folder.InventoryPath, err)
	}

	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for folder destroy %s: %w", folder.InventoryPath, err)
	}

	log.V(2).Info("deleted VM folder", "path", folder.InventoryPath)
	return nil
}

// parseFolderName extracts the folder name from a folderPath. It handles:
//   - Full inventory paths: /{datacenter}/vm/{name} -> {name}
//   - Bare names: {name} -> {name}
//   - Relative paths: {name}/{sub} -> {name}/{sub}
func parseFolderName(datacenter, folderPath string) string {
	folderPath = strings.TrimSpace(folderPath)
	if folderPath == "" {
		return ""
	}

	prefix := fmt.Sprintf("/%s/vm/", datacenter)
	if strings.HasPrefix(folderPath, prefix) {
		return strings.TrimPrefix(folderPath, prefix)
	}

	prefixNoSlash := fmt.Sprintf("%s/vm/", datacenter)
	if strings.HasPrefix(folderPath, prefixNoSlash) {
		return strings.TrimPrefix(folderPath, prefixNoSlash)
	}

	return strings.TrimPrefix(folderPath, "/")
}

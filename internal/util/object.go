package util

// GetObjectKey returns namespace/name, or name for a cluster-scoped object.
func GetObjectKey(name, namespace string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

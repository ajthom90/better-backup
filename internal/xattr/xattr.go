// Package xattr reads and writes the extended attributes of files. On
// Linux they are also where access control lists live: POSIX ACLs in
// system.posix_acl_access and system.posix_acl_default, the NFSv4 ACLs
// of TrueNAS in system.nfs4_acl_xdr, and what Samba keeps for Windows
// clients (DOS attributes, alternate streams) under user.
package xattr

// MaxSize is the most attribute data kept for one file. Attributes
// travel inside the directory listing, so they have to stay small.
const MaxSize = 1 << 20

// nfs4ACL is not always in the list a file gives of its attributes.
const nfs4ACL = "system.nfs4_acl_xdr"

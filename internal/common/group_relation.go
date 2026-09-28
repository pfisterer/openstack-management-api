package common

import "strings"

// RelationMember is the relation every group has: "group:x" means everyone in
// x. The role provider can carry further relations — "group:wwi23seb#dozent" —
// each of which implies membership.
const RelationMember = "member"

// SplitGroupToken splits "group:x#dozent" into ("group:x", "dozent") and
// "group:x" into ("group:x", "member"). A trailing "#member" is dropped, since
// the plain token already means that.
func SplitGroupToken(token string) (groupToken, relation string) {
	if g, rel, ok := strings.Cut(token, "#"); ok && rel != "" {
		return g, rel
	}
	return strings.TrimSuffix(token, "#"), RelationMember
}

// JoinGroupToken is the inverse of SplitGroupToken.
func JoinGroupToken(groupToken, relation string) string {
	if relation == "" || relation == RelationMember {
		return groupToken
	}
	return groupToken + "#" + relation
}

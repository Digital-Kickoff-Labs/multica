package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type archivedMember struct {
	userID, email, role string
}

type accountConflict struct {
	email, archivedID, existingID string
}

func readMembers(dir string, emailMap map[string]string) ([]archivedMember, error) {
	var members []archivedMember
	err := eachLine(dir, "members.jsonl", func(m map[string]any) error {
		members = append(members, archivedMember{userID: str(m, "user_id"), email: mappedEmail(str(m, "email"), emailMap), role: str(m, "role")})
		return nil
	})
	return members, err
}

func mappedEmail(email string, emailMap map[string]string) string {
	if to, ok := emailMap[strings.ToLower(email)]; ok {
		return to
	}
	return email
}

func ownerID(members []archivedMember) string {
	for _, m := range members {
		if m.role == "owner" {
			return m.userID
		}
	}
	if len(members) > 0 {
		return members[0].userID
	}
	return ""
}

// findAccountConflicts lists archived users whose email already belongs to
// another account in the target, e.g. someone who signed in before the restore.
func findAccountConflicts(ctx context.Context, conn *pgx.Conn, members []archivedMember) ([]accountConflict, error) {
	archivedIDs := map[string]string{}
	emails := make([]string, 0, len(members))
	for _, m := range members {
		archivedIDs[m.email] = m.userID
		emails = append(emails, m.email)
	}
	rows, err := conn.Query(ctx, `SELECT email, id::text FROM "user" WHERE email = ANY($1::text[])`, emails)
	if err != nil {
		return nil, fmt.Errorf("check existing accounts: %w", err)
	}
	defer rows.Close()
	var conflicts []accountConflict
	for rows.Next() {
		var c accountConflict
		if err := rows.Scan(&c.email, &c.existingID); err != nil {
			return nil, err
		}
		if c.archivedID = archivedIDs[c.email]; c.archivedID != c.existingID {
			conflicts = append(conflicts, c)
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].email < conflicts[j].email })
	return conflicts, rows.Err()
}

func accountConflictError(conflicts []accountConflict) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d archived account(s) share an email with a different account in the target:\n", len(conflicts))
	for _, c := range conflicts {
		fmt.Fprintf(&b, "  %s: archive %s, target %s\n", c.email, c.archivedID, c.existingID)
	}
	b.WriteString("re-run with -merge-existing-users to attach the imported history to the target accounts, or -email-map OLD=NEW to import them under another address")
	return fmt.Errorf("%s", b.String())
}

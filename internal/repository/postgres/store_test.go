package postgres

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/febrian/areyouai/internal/repository"
	"github.com/lib/pq"
)

func TestNormalizeErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   error
		want error
	}{
		{
			name: "no rows maps to not found",
			in:   sql.ErrNoRows,
			want: repository.ErrNotFound,
		},
		{
			name: "unique violation maps to conflict",
			in:   &pq.Error{Code: "23505"},
			want: repository.ErrConflict,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeErr(tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("normalizeErr() = %v, want %v", got, tc.want)
			}
		})
	}
}

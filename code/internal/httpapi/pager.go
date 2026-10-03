package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
)

// pageSize is how many rows every dashboard list shows per page (step 4.6).
const pageSize = 50

// maxPageParam bounds ?page= so a huge number can't overflow the offset.
const maxPageParam = 1_000_000

// pager is one list's pagination: which page is shown, how many there are,
// and the links to move between them. param is the query parameter that
// carries the page number ("page", or "upage" for the admin's users).
type pager struct {
	Page, Pages, Total int
	Links              []pageLink
}

// pageLink is one entry in a pager's link row; a zero N is a gap ("…").
type pageLink struct {
	N       int
	URL     string
	Label   string
	Current bool
}

// requestedPage reads ?<param>=N: anything missing, malformed or below 1 is
// page 1; huge numbers are capped (and later clamped to the last page).
func requestedPage(r *http.Request, param string) int {
	n, err := strconv.Atoi(r.URL.Query().Get(param))
	if err != nil || n < 1 {
		return 1
	}
	return min(n, maxPageParam)
}

// pages is how many pages total rows take (at least 1, even when empty).
func pages(total int) int {
	return max(1, (total+pageSize-1)/pageSize)
}

// clampPage keeps a requested page within 1..pages(total).
func clampPage(page, total int) int {
	return min(max(page, 1), pages(total))
}

// newPager builds the pager for page (already clamped) of total rows,
// linking back to the same URL with only param changed, so filters such as
// ended=1 and other lists' pages are kept.
func newPager(r *http.Request, param string, page, total int) pager {
	p := pager{Page: page, Pages: pages(total), Total: total}
	if p.Pages == 1 {
		return p
	}
	link := func(n int, label string) pageLink {
		q := r.URL.Query()
		q.Set(param, strconv.Itoa(n))
		return pageLink{N: n, URL: r.URL.Path + "?" + q.Encode(), Label: label, Current: n == page}
	}
	if page > 1 {
		p.Links = append(p.Links, link(page-1, "← Prev"))
	}
	// First, last, and two either side of the current page; "…" for gaps.
	last := 0
	for n := 1; n <= p.Pages; n++ {
		if n != 1 && n != p.Pages && (n < page-2 || n > page+2) {
			continue
		}
		if last != 0 && n > last+1 {
			p.Links = append(p.Links, pageLink{Label: "…"})
		}
		p.Links = append(p.Links, link(n, strconv.Itoa(n)))
		last = n
	}
	if page < p.Pages {
		p.Links = append(p.Links, link(page+1, "Next →"))
	}
	return p
}

// selfURL is the current page's own URL, for its Refresh link.
func selfURL(r *http.Request) string {
	u := url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	return u.String()
}

package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud/pagination"
)

// Iterator is a generic interface for abstracting pagination
// It returns nil when iteration is finished without error.
type Iterator[T any] interface {
	Next() (*T, error)
	Done() bool
}

// sliceIterator implements Iterator for a slice of items
// Useful for APIs that already return full lists.
type sliceIterator[T any] struct {
	items []T
	index int
	err   error
}

// NewSliceIterator creates a new iterator from a slice
func NewSliceIterator[T any](items []T, err error) Iterator[T] {
	return &sliceIterator[T]{
		items: items,
		index: 0,
		err:   err,
	}
}

func (i *sliceIterator[T]) Next() (*T, error) {
	if i.err != nil {
		return nil, i.err
	}
	if i.index >= len(i.items) {
		return nil, nil
	}
	item := i.items[i.index]
	i.index++
	return &item, nil
}

func (i *sliceIterator[T]) Done() bool {
	return i.err != nil || i.index >= len(i.items)
}

// pagerIterator implements Iterator using gophercloud pagers without loading all pages at once.
// It streams items through a channel while paginating lazily.
type pagerIterator[T any] struct {
	items <-chan pagerItem[T]
	done  bool
	err   error
}

type pagerItem[T any] struct {
	item *T
	err  error
}

// NewPagerIterator creates a new generic iterator from a gophercloud Pager.
// Pages are fetched one-by-one to avoid loading all data into memory.
func NewPagerIterator[T any](
	pagerFn func() pagination.Pager,
	extractFn func(pagination.Page) ([]T, error),
) Iterator[T] {
	itemCh := make(chan pagerItem[T], 1)

	go func() {
		defer close(itemCh)

		pager := pagerFn()
		err := pager.EachPage(func(page pagination.Page) (bool, error) {
			items, err := extractFn(page)
			if err != nil {
				itemCh <- pagerItem[T]{err: fmt.Errorf("failed to extract items: %w", err)}
				return false, err
			}

			for _, item := range items {
				itemCopy := item // avoid taking address of loop variable
				itemCh <- pagerItem[T]{item: &itemCopy}
			}
			return true, nil
		})

		if err != nil {
			itemCh <- pagerItem[T]{err: fmt.Errorf("failed to fetch page: %w", err)}
		}
	}()

	return &pagerIterator[T]{items: itemCh}
}

func (i *pagerIterator[T]) Next() (*T, error) {
	if i.done {
		return nil, i.err
	}

	item, ok := <-i.items
	if !ok {
		i.done = true
		return nil, i.err
	}

	if item.err != nil {
		i.done = true
		i.err = item.err
		return nil, item.err
	}

	return item.item, nil
}

func (i *pagerIterator[T]) Done() bool {
	return i.done || i.err != nil
}

// CollectIterator drains an Iterator into a slice for callers that need all items.
// It still paginates lazily under the hood.
func CollectIterator[T any](iter Iterator[T]) ([]T, error) {
	var out []T
	for {
		item, err := iter.Next()
		if err != nil {
			return nil, err
		}
		if item == nil {
			return out, nil
		}
		out = append(out, *item)
	}
}

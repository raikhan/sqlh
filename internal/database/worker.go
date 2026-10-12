package database

import (
	"context"
	"log"
	"sync"
)

type Worker struct {
	dbRepo  DBRepository
	dbCache *DBCache

	done     chan struct{}
	update   chan struct{}
	lock     sync.Mutex
	stopOnce sync.Once

	// OnChange, if set, runs (in its own goroutine) after the cache changed.
	OnChange func()
}

func NewWorker() *Worker {
	return &Worker{
		done:   make(chan struct{}, 1),
		update: make(chan struct{}, 1),
	}
}

func (w *Worker) Cache() *DBCache {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.dbCache
}

func (w *Worker) setCache(c *DBCache) {
	w.lock.Lock()
	w.dbCache = c
	w.lock.Unlock()
	w.changed()
}

func (w *Worker) changed() {
	if w.OnChange != nil {
		go w.OnChange()
	}
}

func (w *Worker) setSecondary(col map[string][]*ColumnDesc, synonyms map[string][]string) {
	w.lock.Lock()
	if w.dbCache != nil {
		// Swap in a copy so that readers holding the previous
		// *DBCache keep seeing a consistent snapshot.
		newCache := *w.dbCache
		newCache.ColumnsWithParent = col
		newCache.Synonyms = synonyms
		newCache.SecondaryLoaded = true
		w.dbCache = &newCache
	}
	w.lock.Unlock()
	w.changed()
}

func (w *Worker) Start() {
	go func() {
		log.Println("db worker: start")
		for {
			select {
			case <-w.done:
				log.Println("db worker: done")
				return
			case <-w.update:
				generator := NewDBCacheUpdater(w.dbRepo)
				col, err := generator.GenerateDBCacheSecondary(context.Background())
				if err != nil {
					log.Println(err)
					continue
				}
				var synonyms map[string][]string
				if sl, ok := w.dbRepo.(SynonymLister); ok {
					if synonyms, err = sl.Synonyms(context.Background()); err != nil {
						log.Println("synonyms:", err) // lint stays unsure about unqualified names
						synonyms = nil
					}
				}
				w.setSecondary(col, synonyms)
				log.Println("db worker: Update db cache secondary complete")
			}
		}
	}()
}

func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
}

// SetRepository replaces the repository used for background cache updates.
func (w *Worker) SetRepository(repo DBRepository) {
	w.dbRepo = repo
}

func (w *Worker) ReCache(ctx context.Context, repo DBRepository) error {
	w.dbRepo = repo
	if err := w.updateAllCache(ctx); err != nil {
		return err
	}
	w.updateAdditionalCache()
	return nil
}

func (w *Worker) updateAllCache(ctx context.Context) error {
	generator := NewDBCacheUpdater(w.dbRepo)
	cache, err := generator.GenerateDBCachePrimary(ctx)
	if err != nil {
		return err
	}
	w.setCache(cache)
	log.Println("db worker: Update db cache primary complete")
	return nil
}

func (w *Worker) updateAdditionalCache() {
	w.update <- struct{}{}
}

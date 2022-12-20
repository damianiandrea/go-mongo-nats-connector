package connector

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/exp/slog"
	"golang.org/x/sync/errgroup"

	"github.com/damianiandrea/go-mongo-nats-connector/internal/config"
	"github.com/damianiandrea/go-mongo-nats-connector/internal/health"
	"github.com/damianiandrea/go-mongo-nats-connector/internal/mongo"
	"github.com/damianiandrea/go-mongo-nats-connector/internal/nats"
)

type Connector struct {
	ctx  context.Context
	stop context.CancelFunc

	cfg         *config.Config
	logger      *slog.Logger
	mongoClient *mongo.Client
	natsClient  *nats.Client
	server      *http.Server
}

func New(cfg *config.Config) (*Connector, error) {
	logLevel := convertLogLevel(cfg.Connector.Log.Level)
	loggerOpts := &slog.HandlerOptions{Level: logLevel}
	logger := slog.New(loggerOpts.NewJSONHandler(os.Stdout))

	mongoClient, err := mongo.NewClient(logger, mongo.WithMongoUri(cfg.Connector.Mongo.Uri))
	if err != nil {
		return nil, err
	}

	natsClient, err := nats.NewClient(logger, nats.WithNatsUrl(cfg.Connector.Nats.Url))
	if err != nil {
		return nil, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	mux := http.NewServeMux()
	mux.Handle("/healthz", health.NewHandler(mongoClient, natsClient))
	server := &http.Server{
		Addr:    cfg.Connector.Addr,
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
	}

	return &Connector{
		ctx:         ctx,
		stop:        stop,
		cfg:         cfg,
		logger:      logger,
		mongoClient: mongoClient,
		natsClient:  natsClient,
		server:      server,
	}, nil
}

func (c *Connector) Run() error {
	defer closeClient(c.mongoClient)
	defer closeClient(c.natsClient)
	defer c.stop()

	group, groupCtx := errgroup.WithContext(c.ctx)

	collCreator := mongo.NewCollectionCreator(c.mongoClient, c.logger)
	streamAdder := nats.NewStreamAdder(c.natsClient, c.logger)
	streamPublisher := nats.NewStreamPublisher(c.natsClient, c.logger)

	for _, _coll := range c.cfg.Connector.Collections {
		coll := _coll // to avoid unexpected behavior
		createWatchedCollOpts := &mongo.CreateCollectionOptions{
			DbName:                       coll.DbName,
			CollName:                     coll.CollName,
			ChangeStreamPreAndPostImages: *coll.ChangeStreamPreAndPostImages,
		}
		if err := collCreator.CreateCollection(groupCtx, createWatchedCollOpts); err != nil {
			return err
		}

		createResumeTokensCollOpts := &mongo.CreateCollectionOptions{
			DbName:      coll.TokensDbName,
			CollName:    coll.TokensCollName,
			Capped:      *coll.TokensCollCapped,
			SizeInBytes: *coll.TokensCollSize,
		}
		if err := collCreator.CreateCollection(groupCtx, createResumeTokensCollOpts); err != nil {
			return err
		}

		if err := streamAdder.AddStream(coll.StreamName); err != nil {
			return err
		}

		group.Go(func() error {
			watcher := mongo.NewCollectionWatcher(c.mongoClient, c.logger, mongo.WithChangeStreamHandler(streamPublisher.Publish))
			watchCollOpts := &mongo.WatchCollectionOptions{
				WatchedDbName:        coll.DbName,
				WatchedCollName:      coll.CollName,
				ResumeTokensDbName:   coll.TokensDbName,
				ResumeTokensCollName: coll.TokensCollName,
			}
			return watcher.WatchCollection(groupCtx, watchCollOpts) // blocking call
		})
	}

	group.Go(func() error {
		c.logger.Info("connector started", "addr", c.server.Addr)
		return c.server.ListenAndServe()
	})

	group.Go(func() error {
		<-groupCtx.Done()
		c.logger.Info("connector gracefully shutting down", "addr", c.server.Addr)
		return c.server.Shutdown(context.Background())
	})

	return group.Wait()
}

func convertLogLevel(logLevel string) slog.Level {
	switch strings.ToLower(logLevel) {
	case "debug":
		return slog.DebugLevel
	case "warn":
		return slog.WarnLevel
	case "error":
		return slog.ErrorLevel
	case "info":
		fallthrough
	default:
		return slog.InfoLevel
	}
}

func closeClient(closer io.Closer) {
	if err := closer.Close(); err != nil {
		log.Printf("%v", err)
	}
}

package main

// Check de MongoDB: ping no primário + versão (buildInfo) + bancos/tamanhos.

import (
	"context"
	"net/url"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// mongoURI monta a URI; com usuário, autentica no banco informado (default admin).
func mongoURI(c agentCheck) string {
	u := &url.URL{Scheme: "mongodb", Host: c.addr()}
	if c.Username != "" {
		u.User = url.UserPassword(c.Username, c.Password)
		q := url.Values{}
		auth := c.DatabaseName
		if auth == "" {
			auth = "admin"
		}
		q.Set("authSource", auth)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// mongoSession é o que o check usa de uma conexão Mongo.
type mongoSession interface {
	Ping(ctx context.Context) error
	BuildVersion(ctx context.Context) string
	Databases(ctx context.Context) ([]dbEntry, error)
	Disconnect(ctx context.Context) error
}

type mongoConnectFunc func(ctx context.Context, uri string) (mongoSession, error)

func mongoProbe(connect mongoConnectFunc, uri string) (*checkDetails, error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	s, err := connect(ctx, uri)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Disconnect(context.Background()) }()
	if err := s.Ping(ctx); err != nil {
		return nil, err
	}
	d := &checkDetails{Version: s.BuildVersion(ctx)}
	if dbs, err := s.Databases(ctx); err == nil {
		d.Databases = dbs
	}
	return finishDetails(d), nil
}

// driverSession implementa mongoSession com o driver oficial.
type driverSession struct{ c *mongo.Client }

func connectMongo(ctx context.Context, uri string) (mongoSession, error) {
	c, err := mongo.Connect(ctx, options.Client().
		ApplyURI(uri).
		SetConnectTimeout(checkTimeout).
		SetServerSelectionTimeout(checkTimeout))
	if err != nil {
		return nil, err
	}
	return driverSession{c}, nil
}

func (s driverSession) Ping(ctx context.Context) error {
	return s.c.Ping(ctx, readpref.Primary())
}

func (s driverSession) BuildVersion(ctx context.Context) string {
	var bi struct {
		Version string `bson:"version"`
	}
	_ = s.c.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&bi)
	return bi.Version
}

func (s driverSession) Databases(ctx context.Context) ([]dbEntry, error) {
	res, err := s.c.ListDatabases(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	out := make([]dbEntry, 0, len(res.Databases))
	for _, dbi := range res.Databases {
		out = append(out, dbEntry{Name: dbi.Name, SizeBytes: dbi.SizeOnDisk})
	}
	return out, nil
}

func (s driverSession) Disconnect(ctx context.Context) error {
	return s.c.Disconnect(ctx)
}

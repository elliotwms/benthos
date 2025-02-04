package processor

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Jeffail/benthos/v3/internal/bloblang"
	"github.com/Jeffail/benthos/v3/internal/bloblang/field"
	"github.com/Jeffail/benthos/v3/internal/docs"
	bredis "github.com/Jeffail/benthos/v3/internal/service/redis"
	"github.com/Jeffail/benthos/v3/lib/log"
	"github.com/Jeffail/benthos/v3/lib/metrics"
	"github.com/Jeffail/benthos/v3/lib/types"
	"github.com/go-redis/redis/v7"
	"github.com/opentracing/opentracing-go"
)

//------------------------------------------------------------------------------

func init() {
	Constructors[TypeRedis] = TypeSpec{
		constructor: NewRedis,
		Categories: []Category{
			CategoryIntegration,
		},
		Summary: `
Performs actions against Redis that aren't possible using a
` + "[`cache`](/docs/components/processors/cache)" + ` processor. Actions are
performed for each message of a batch, where the contents are replaced with the
result.`,
		Description: `
## Operators

### ` + "`scard`" + `

Returns the cardinality of a set, or ` + "`0`" + ` if the key does not exist.

### ` + "`sadd`" + `

Adds a new member to a set. Returns ` + "`1`" + ` if the member was added.`,
		FieldSpecs: bredis.ConfigDocs().Add(
			docs.FieldCommon("operator", "The [operator](#operators) to apply.").HasOptions("scard", "sadd"),
			docs.FieldCommon("key", "A key to use for the target operator.").IsInterpolated(),
			docs.FieldAdvanced("retries", "The maximum number of retries before abandoning a request."),
			docs.FieldAdvanced("retry_period", "The time to wait before consecutive retry attempts."),
			PartsFieldSpec,
		),
		Examples: []docs.AnnotatedExample{
			{
				Title: "Querying Cardinality",
				Summary: `
If given payloads containing a metadata field ` + "`set_key`" + ` it's possible
to query and store the cardinality of the set for each message using a
` + "[`branch` processor](/docs/components/processors/branch)" + ` in order to
augment rather than replace the message contents:`,
				Config: `
pipeline:
  processors:
    - branch:
        processors:
          - redis:
              url: TODO
              operator: scard
              key: ${! meta("set_key") }
        result_map: 'root.cardinality = this'
`,
			},
		},
	}
}

//------------------------------------------------------------------------------

// RedisConfig contains configuration fields for the Redis processor.
type RedisConfig struct {
	bredis.Config `json:",inline" yaml:",inline"`
	Parts         []int  `json:"parts" yaml:"parts"`
	Operator      string `json:"operator" yaml:"operator"`
	Key           string `json:"key" yaml:"key"`
	Retries       int    `json:"retries" yaml:"retries"`
	RetryPeriod   string `json:"retry_period" yaml:"retry_period"`
}

// NewRedisConfig returns a RedisConfig with default values.
func NewRedisConfig() RedisConfig {
	return RedisConfig{
		Config:      bredis.NewConfig(),
		Parts:       []int{},
		Operator:    "scard",
		Key:         "",
		Retries:     3,
		RetryPeriod: "500ms",
	}
}

//------------------------------------------------------------------------------

// Redis is a processor that performs redis operations
type Redis struct {
	parts []int
	conf  Config
	log   log.Modular
	stats metrics.Type

	key *field.Expression

	operator    redisOperator
	client      redis.UniversalClient
	retryPeriod time.Duration

	mCount      metrics.StatCounter
	mErr        metrics.StatCounter
	mSent       metrics.StatCounter
	mBatchSent  metrics.StatCounter
	mRedisRetry metrics.StatCounter
}

// NewRedis returns a Redis processor.
func NewRedis(
	conf Config, mgr types.Manager, log log.Modular, stats metrics.Type,
) (Type, error) {
	var retryPeriod time.Duration
	if tout := conf.Redis.RetryPeriod; len(tout) > 0 {
		var err error
		if retryPeriod, err = time.ParseDuration(tout); err != nil {
			return nil, fmt.Errorf("failed to parse retry period string: %v", err)
		}
	}

	client, err := conf.Redis.Config.Client()
	if err != nil {
		return nil, err
	}

	key, err := bloblang.NewField(conf.Redis.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key expression: %v", err)
	}

	r := &Redis{
		parts: conf.Redis.Parts,
		conf:  conf,
		log:   log,
		stats: stats,

		key: key,

		retryPeriod: retryPeriod,
		client:      client,

		mCount:      stats.GetCounter("count"),
		mErr:        stats.GetCounter("error"),
		mSent:       stats.GetCounter("sent"),
		mBatchSent:  stats.GetCounter("batch.sent"),
		mRedisRetry: stats.GetCounter("redis.retry"),
	}

	if r.operator, err = getRedisOperator(conf.Redis.Operator); err != nil {
		return nil, err
	}
	return r, nil
}

//------------------------------------------------------------------------------

type redisOperator func(r *Redis, key string, value []byte) ([]byte, error)

func newRedisSCardOperator() redisOperator {
	return func(r *Redis, key string, value []byte) ([]byte, error) {
		res, err := r.client.SCard(key).Result()

		for i := 0; i <= r.conf.Redis.Retries && err != nil; i++ {
			r.log.Errorf("SCard command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			r.mRedisRetry.Incr(1)
			res, err = r.client.SCard(key).Result()
		}

		if err != nil {
			return nil, err
		}
		return strconv.AppendInt(nil, res, 10), nil
	}
}

func newRedisSAddOperator() redisOperator {
	return func(r *Redis, key string, value []byte) ([]byte, error) {
		res, err := r.client.SAdd(key, value).Result()

		for i := 0; i <= r.conf.Redis.Retries && err != nil; i++ {
			r.log.Errorf("SCard command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			r.mRedisRetry.Incr(1)
			res, err = r.client.SAdd(key, value).Result()
		}

		if err != nil {
			return nil, err
		}
		return strconv.AppendInt(nil, res, 10), nil
	}
}

func getRedisOperator(opStr string) (redisOperator, error) {
	switch opStr {
	case "sadd":
		return newRedisSAddOperator(), nil
	case "scard":
		return newRedisSCardOperator(), nil
	}
	return nil, fmt.Errorf("operator not recognised: %v", opStr)
}

// ProcessMessage applies the processor to a message, either creating >0
// resulting messages or a response to be sent back to the message source.
func (r *Redis) ProcessMessage(msg types.Message) ([]types.Message, types.Response) {
	r.mCount.Incr(1)
	newMsg := msg.Copy()

	proc := func(index int, span opentracing.Span, part types.Part) error {
		key := r.key.String(index, newMsg)
		res, err := r.operator(r, key, part.Get())
		if err != nil {
			r.mErr.Incr(1)
			r.log.Debugf("Operator failed for key '%s': %v\n", key, err)
			return err
		}
		part.Set(res)
		return nil
	}

	IteratePartsWithSpan(TypeRedis, r.parts, newMsg, proc)

	r.mBatchSent.Incr(1)
	r.mSent.Incr(int64(newMsg.Len()))
	return []types.Message{newMsg}, nil
}

// CloseAsync shuts down the processor and stops processing requests.
func (r *Redis) CloseAsync() {
}

// WaitForClose blocks until the processor has closed down.
func (r *Redis) WaitForClose(timeout time.Duration) error {
	r.client.Close()
	return nil
}

//------------------------------------------------------------------------------

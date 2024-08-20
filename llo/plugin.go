package llo

import (
	"context"
	"fmt"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// TODO: Split out this file and write unit tests: https://smartcontract-it.atlassian.net/browse/MERC-3524

// Additional limits so we can more effectively bound the size of observations
// NOTE: These are hardcoded because these exact values are relied upon as a
// property of coming to consensus, it's too dangerous to make these
// configurable on a per-node basis. It may be possible to add them to the
// OffchainConfig if they need to be changed dynamically and in a
// backwards-compatible way.
const (
	// Maximum amount of channels that can be added per round (if more than
	// this needs to be added, it will be added in batches until everything is
	// up-to-date)
	MaxObservationRemoveChannelIDsLength = 5
	// Maximum amount of channels that can be removed per round (if more than
	// this needs to be removed, it will be removed in batches until everything
	// is up-to-date)
	MaxObservationUpdateChannelDefinitionsLength = 5
	// Maximum number of streams that can be observed per round
	// TODO: This needs to be implemented on the Observation side so we don't
	// even generate an observation that fails this
	MaxObservationStreamValuesLength = 10_000
	// MaxOutcomeChannelDefinitionsLength is the maximum number of channels that
	// can be supported
	// TODO: This needs to be implemented on the Observation side so we don't
	// even generate an observation that fails this
	MaxOutcomeChannelDefinitionsLength = 10_000
)

type DSOpts interface {
	VerboseLogging() bool
	SeqNr() uint64
}

type dsOpts struct {
	verboseLogging bool
	seqNr          uint64
}

func (o dsOpts) VerboseLogging() bool {
	return o.verboseLogging
}

func (o dsOpts) SeqNr() uint64 {
	return o.seqNr
}

type DataSource interface {
	// For each known streamID, Observe should set the observed value in the
	// passed streamValues.
	// If an observation fails, or the stream is unknown, no value should be
	// set.
	Observe(ctx context.Context, streamValues StreamValues, opts DSOpts) error
}

// Protocol instances start in either the staging or production stage. They
// may later be retired and "hand over" their work to another protocol instance
// that will move from the staging to the production stage.
const (
	LifeCycleStageStaging    llotypes.LifeCycleStage = "staging"
	LifeCycleStageProduction llotypes.LifeCycleStage = "production"
	LifeCycleStageRetired    llotypes.LifeCycleStage = "retired"
)

type RetirementReport struct {
	// Carries validity time stamps between protocol instances to ensure there
	// are no gaps
	ValidAfterSeconds map[llotypes.ChannelID]uint32
}

type ShouldRetireCache interface { // reads asynchronously from onchain ConfigurationStore
	// Should the protocol instance retire according to the configuration
	// contract?
	// See: https://github.com/smartcontractkit/mercury-v1-sketch/blob/main/onchain/src/ConfigurationStore.sol#L18
	ShouldRetire() (bool, error)
}

// The predecessor protocol instance stores its attested retirement report in
// this cache (locally, offchain), so it can be fetched by the successor
// protocol instance.
//
// PredecessorRetirementReportCache is populated by the old protocol instance
// writing to it and the new protocol instance reading from it.
//
// The sketch envisions it being implemented as a single object that is shared
// between different protocol instances.
type PredecessorRetirementReportCache interface {
	AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error)
	CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, attestedRetirementReport []byte) (RetirementReport, error)
}

type ChannelDefinitionCache interface {
	Definitions() llotypes.ChannelDefinitions
}

// A ReportingPlugin allows plugging custom logic into the OCR3 protocol. The OCR
// protocol handles cryptography, networking, ensuring that a sufficient number
// of nodes is in agreement about any report, transmitting the report to the
// contract, etc... The ReportingPlugin handles application-specific logic. To do so,
// the ReportingPlugin defines a number of callbacks that are called by the OCR
// protocol logic at certain points in the protocol's execution flow. The report
// generated by the ReportingPlugin must be in a format understood by contract that
// the reports are transmitted to.
//
// We assume that each correct node participating in the protocol instance will
// be running the same ReportingPlugin implementation. However, not all nodes may be
// correct; up to f nodes be faulty in arbitrary ways (aka byzantine faults).
// For example, faulty nodes could be down, have intermittent connectivity
// issues, send garbage messages, or be controlled by an adversary.
//
// For a protocol round where everything is working correctly, followers will
// call Observation, Outcome, and Reports. For each report,
// ShouldAcceptAttestedReport will be called as well. If
// ShouldAcceptAttestedReport returns true, ShouldTransmitAcceptedReport will
// be called. However, an ReportingPlugin must also correctly handle the case where
// faults occur.
//
// In particular, an ReportingPlugin must deal with cases where:
//
// - only a subset of the functions on the ReportingPlugin are invoked for a given
// round
//
// - an arbitrary number of seqnrs has been skipped between invocations of the
// ReportingPlugin
//
// - the observation returned by Observation is not included in the list of
// AttributedObservations passed to Report
//
// - a query or observation is malformed. (For defense in depth, it is also
// recommended that malformed outcomes are handled gracefully.)
//
// - instances of the ReportingPlugin run by different oracles have different call
// traces. E.g., the ReportingPlugin's Observation function may have been invoked on
// node A, but not on node B.
//
// All functions on an ReportingPlugin should be thread-safe.
//
// All functions that take a context as their first argument may still do cheap
// computations after the context expires, but should stop any blocking
// interactions with outside services (APIs, database, ...) and return as
// quickly as possible. (Rough rule of thumb: any such computation should not
// take longer than a few ms.) A blocking function may block execution of the
// entire protocol instance on its node!
//
// For a given OCR protocol instance, there can be many (consecutive) instances
// of an ReportingPlugin, e.g. due to software restarts. If you need ReportingPlugin state
// to survive across restarts, you should store it in the Outcome or persist it.
// A ReportingPlugin instance will only ever serve a single protocol instance.
var _ ocr3types.ReportingPluginFactory[llotypes.ReportInfo] = &PluginFactory{}

func NewPluginFactory(cfg Config, prrc PredecessorRetirementReportCache, src ShouldRetireCache, cdc ChannelDefinitionCache, ds DataSource, lggr logger.Logger, codecs map[llotypes.ReportFormat]ReportCodec) *PluginFactory {
	return &PluginFactory{
		cfg, prrc, src, cdc, ds, lggr, codecs,
	}
}

type Config struct {
	// Enables additional logging that might be expensive, e.g. logging entire
	// channel definitions on every round or other very large structs
	VerboseLogging bool
}

type PluginFactory struct {
	Config                           Config
	PredecessorRetirementReportCache PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	Codecs                           map[llotypes.ReportFormat]ReportCodec
}

func (f *PluginFactory) NewReportingPlugin(cfg ocr3types.ReportingPluginConfig) (ocr3types.ReportingPlugin[llotypes.ReportInfo], ocr3types.ReportingPluginInfo, error) {
	offchainCfg, err := DecodeOffchainConfig(cfg.OffchainConfig)
	if err != nil {
		return nil, ocr3types.ReportingPluginInfo{}, fmt.Errorf("NewReportingPlugin failed to decode offchain config; got: 0x%x (len: %d); %w", cfg.OffchainConfig, len(cfg.OffchainConfig), err)
	}

	return &Plugin{
			f.Config,
			offchainCfg.PredecessorConfigDigest,
			cfg.ConfigDigest,
			f.PredecessorRetirementReportCache,
			f.ShouldRetireCache,
			f.ChannelDefinitionCache,
			f.DataSource,
			f.Logger,
			cfg.F,
			protoObservationCodec{},
			protoOutcomeCodec{},
			f.Codecs,
		}, ocr3types.ReportingPluginInfo{
			Name: "LLO",
			Limits: ocr3types.ReportingPluginLimits{
				MaxQueryLength:       0,
				MaxObservationLength: ocr3types.MaxMaxObservationLength, // TODO: use tighter bound MERC-3524
				MaxOutcomeLength:     ocr3types.MaxMaxOutcomeLength,     // TODO: use tighter bound MERC-3524
				MaxReportLength:      ocr3types.MaxMaxReportLength,      // TODO: use tighter bound MERC-3524
				MaxReportCount:       ocr3types.MaxMaxReportCount,       // TODO: use tighter bound MERC-3524
			},
		}, nil
}

var _ ocr3types.ReportingPlugin[llotypes.ReportInfo] = &Plugin{}

type ReportCodec interface {
	// Encode may be lossy, so no Decode function is expected
	// Encode should handle nil stream aggregate values without panicking (it
	// may return error instead)
	Encode(Report, llotypes.ChannelDefinition) ([]byte, error)
}

type Plugin struct {
	Config                           Config
	PredecessorConfigDigest          *types.ConfigDigest
	ConfigDigest                     types.ConfigDigest
	PredecessorRetirementReportCache PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	F                                int
	ObservationCodec                 ObservationCodec
	OutcomeCodec                     OutcomeCodec
	Codecs                           map[llotypes.ReportFormat]ReportCodec
}

// Query creates a Query that is sent from the leader to all follower nodes
// as part of the request for an observation. Be careful! A malicious leader
// could equivocate (i.e. send different queries to different followers.)
// Many applications will likely be better off always using an empty query
// if the oracles don't need to coordinate on what to observe (e.g. in case
// of a price feed) or the underlying data source offers an (eventually)
// consistent view to different oracles (e.g. in case of observing a
// blockchain).
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *Plugin) Query(ctx context.Context, outctx ocr3types.OutcomeContext) (types.Query, error) {
	return nil, nil
}

// Observation gets an observation from the underlying data source. Returns
// a value or an error.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
//
// Should return a serialized Observation struct.
func (p *Plugin) Observation(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query) (types.Observation, error) {
	return p.observation(ctx, outctx, query)
}

// Should return an error if an observation isn't well-formed.
// Non-well-formed  observations will be discarded by the protocol. This is
// called for each observation, don't do anything slow in here.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *Plugin) ValidateObservation(outctx ocr3types.OutcomeContext, query types.Query, ao types.AttributedObservation) error {
	if outctx.SeqNr < 1 {
		return fmt.Errorf("Invalid SeqNr: %d", outctx.SeqNr)
	} else if outctx.SeqNr == 1 {
		if len(ao.Observation) != 0 {
			return fmt.Errorf("Expected empty observation for first round, got: 0x%x", ao.Observation)
		}
	}

	observation, err := p.ObservationCodec.Decode(ao.Observation)
	if err != nil {
		// Critical error
		// If the previous outcome cannot be decoded for whatever reason, the
		// protocol will become permanently stuck at this point
		return fmt.Errorf("Observation decode error (got: 0x%x): %w", ao.Observation, err)
	}

	if p.PredecessorConfigDigest == nil && len(observation.AttestedPredecessorRetirement) != 0 {
		return fmt.Errorf("AttestedPredecessorRetirement is not empty even though this instance has no predecessor")
	}

	if len(observation.UpdateChannelDefinitions) > MaxObservationUpdateChannelDefinitionsLength {
		return fmt.Errorf("UpdateChannelDefinitions is too long: %v vs %v", len(observation.UpdateChannelDefinitions), MaxObservationUpdateChannelDefinitionsLength)
	}

	if len(observation.RemoveChannelIDs) > MaxObservationRemoveChannelIDsLength {
		return fmt.Errorf("RemoveChannelIDs is too long: %v vs %v", len(observation.RemoveChannelIDs), MaxObservationRemoveChannelIDsLength)
	}

	if err := VerifyChannelDefinitions(observation.UpdateChannelDefinitions); err != nil {
		return fmt.Errorf("UpdateChannelDefinitions is invalid: %w", err)
	}

	if len(observation.StreamValues) > MaxObservationStreamValuesLength {
		return fmt.Errorf("StreamValues is too long: %v vs %v", len(observation.StreamValues), MaxObservationStreamValuesLength)
	}

	return nil
}

// Generates an outcome for a seqNr, typically based on the previous
// outcome, the current query, and the current set of attributed
// observations.
//
// This function should be pure. Don't do anything slow in here.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
//
// libocr guarantees that this will always be called with at least 2f+1
// AttributedObservations
func (p *Plugin) Outcome(outctx ocr3types.OutcomeContext, query types.Query, aos []types.AttributedObservation) (ocr3types.Outcome, error) {
	return p.outcome(outctx, query, aos)
}

// Generates a (possibly empty) list of reports from an outcome. Each report
// will be signed and possibly be transmitted to the contract. (Depending on
// ShouldAcceptAttestedReport & ShouldTransmitAcceptedReport)
//
// This function should be pure. Don't do anything slow in here.
//
// This is likely to change in the future. It will likely be returning a
// list of report batches, where each batch goes into its own Merkle tree.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *Plugin) Reports(seqNr uint64, rawOutcome ocr3types.Outcome) ([]ocr3types.ReportWithInfo[llotypes.ReportInfo], error) {
	return p.reports(seqNr, rawOutcome)
}

func (p *Plugin) ShouldAcceptAttestedReport(context.Context, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo]) (bool, error) {
	// Transmit it all to the Mercury server
	return true, nil
}

func (p *Plugin) ShouldTransmitAcceptedReport(context.Context, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo]) (bool, error) {
	// Transmit it all to the Mercury server
	return true, nil
}

// ObservationQuorum returns the minimum number of valid (according to
// ValidateObservation) observations needed to construct an outcome.
//
// This function should be pure. Don't do anything slow in here.
//
// This is an advanced feature. The "default" approach (what OCR1 & OCR2
// did) is to have an empty ValidateObservation function and return
// QuorumTwoFPlusOne from this function.
func (p *Plugin) ObservationQuorum(outctx ocr3types.OutcomeContext, query types.Query) (ocr3types.Quorum, error) {
	return ocr3types.QuorumTwoFPlusOne, nil
}

func (p *Plugin) Close() error {
	return nil
}

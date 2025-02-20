package llo

import (
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func VerifyChannelDefinitions(channelDefs llotypes.ChannelDefinitions) error {
	if len(channelDefs) > MaxOutcomeChannelDefinitionsLength {
		return fmt.Errorf("too many channels, got: %d/%d", len(channelDefs), MaxOutcomeChannelDefinitionsLength)
	}
	uniqueStreamIDs := make(map[llotypes.StreamID]struct{}, len(channelDefs))
	for channelID, cd := range channelDefs {
		if len(cd.Streams) == 0 {
			return fmt.Errorf("ChannelDefinition with ID %d has no streams", channelID)
		}
		for _, strm := range cd.Streams {
			if strm.Aggregator == 0 {
				return fmt.Errorf("ChannelDefinition with ID %d has stream %d with zero aggregator (this may indicate an uninitialized struct)", channelID, strm.StreamID)
			}
			uniqueStreamIDs[strm.StreamID] = struct{}{}
		}
		switch cd.ReportFormat {
		case llotypes.ReportFormatEVMPremiumLegacy:
			if err := VerifyEVMPremiumLegacyChannelDefinition(cd); err != nil {
				return fmt.Errorf("invalid ChannelDefinition with ID %d: %v", channelID, err)
			}
		default:
			// NOTE: Could add further report-format-specific validation here
			// for future report formats
			//
			// Generally speaking we are lenient here since we don't know what
			// future report codecs will want, so we defer and let the report
			// codec error out on encode if the Opts are invalid
		}
	}
	if len(uniqueStreamIDs) > MaxObservationStreamValuesLength {
		return fmt.Errorf("too many unique stream IDs, got: %d/%d", len(uniqueStreamIDs), MaxObservationStreamValuesLength)
	}
	return nil
}

func VerifyEVMPremiumLegacyChannelDefinition(cd llotypes.ChannelDefinition) error {
	if cd.ReportFormat != llotypes.ReportFormatEVMPremiumLegacy {
		return fmt.Errorf("expected ReportFormatEVMPremiumLegacy, got: %v", cd.ReportFormat)
	}
	if len(cd.Streams) != 3 {
		return fmt.Errorf("ReportFormatEVMPremiumLegacy requires exactly 3 streams (NativePrice, LinkPrice, Quote); got: %v", cd.Streams)
	}
	return nil
}

func subtractChannelDefinitions(minuend llotypes.ChannelDefinitions, subtrahend llotypes.ChannelDefinitions, limit int) llotypes.ChannelDefinitions {
	differenceList := []ChannelDefinitionWithID{}
	for channelID, channelDefinition := range minuend {
		if _, ok := subtrahend[channelID]; !ok {
			differenceList = append(differenceList, ChannelDefinitionWithID{channelDefinition, channelID})
		}
	}

	// Sort so we return deterministic result
	sort.Slice(differenceList, func(i, j int) bool {
		return differenceList[i].ChannelID < differenceList[j].ChannelID
	})

	if len(differenceList) > limit {
		differenceList = differenceList[:limit]
	}

	difference := llotypes.ChannelDefinitions{}
	for _, defWithID := range differenceList {
		difference[defWithID.ChannelID] = defWithID.ChannelDefinition
	}

	return difference
}

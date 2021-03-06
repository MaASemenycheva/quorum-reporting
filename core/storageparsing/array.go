package storageparsing

import (
	"strconv"
	"strings"

	"quorumengineering/quorum-report/types"
)

func (p *Parser) ParseArray(entry types.SolidityStorageEntry, namedType types.SolidityTypeEntry) ([]interface{}, error) {
	isDynamic := namedType.Encoding == "dynamic_array"
	sizeOfArray, err := p.determineSize(entry, isDynamic)
	if err != nil {
		return nil, err
	}

	sizeOfElement := p.template.Types[namedType.Base].NumberOfBytes

	storageSlot := p.ResolveSlot(bigN(entry.Slot))
	if isDynamic {
		storageSlot = hash(storageSlot)
	}

	newTemplate := p.createArrayStorageDocument(sizeOfArray, sizeOfElement, namedType.Base)

	arrayParser := NewParser(p.storageManager, newTemplate, storageSlot)
	out, err := arrayParser.ParseRawStorage()
	if err != nil {
		return nil, err
	}
	extractedResults := make([]interface{}, 0, len(out))
	for _, result := range out {
		extractedResults = append(extractedResults, result.Value)
	}
	return extractedResults, nil
}

func (p *Parser) createArrayStorageDocument(sizeOfArray uint64, sizeOfElement uint64, baseType string) types.SolidityStorageDocument {
	//build up array of fake storage elements the array has
	storageElements := make([]types.SolidityStorageEntry, 0)

	currentSlot := uint64(0)
	currentOffset := uint64(0)
	for i := uint64(0); i < sizeOfArray; i++ {

		nextEntry := types.SolidityStorageEntry{
			Offset: currentOffset,
			Slot:   currentSlot,
			Type:   baseType,
		}
		storageElements = append(storageElements, nextEntry)

		currentOffset += sizeOfElement
		if currentOffset+sizeOfElement > 32 {
			currentSlot += roundUpTo32(currentOffset) / 32
			currentOffset = 0
		}
	}

	return types.SolidityStorageDocument{
		Storage: storageElements,
		Types:   p.template.Types,
	}
}

func (p *Parser) determineSize(storageItem types.SolidityStorageEntry, isDynamic bool) (uint64, error) {
	if isDynamic {
		storageSlot := p.ResolveSlot(bigN(storageItem.Slot))
		extracted := ExtractFromSingleStorage(0, 32, p.storageManager.Get(storageSlot))
		numberOfElements := p.ParseUint(extracted).Uint64()
		return numberOfElements, nil
	}

	name := storageItem.Type
	// determine the position the size starts from
	startOfAmount := strings.LastIndex(name, ")")
	endOfAmount := strings.LastIndex(name, "_")
	size := name[startOfAmount+1 : endOfAmount]

	return strconv.ParseUint(size, 10, 0)
}

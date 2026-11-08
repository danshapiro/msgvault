package pst

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
)

// The four-byte inline property-context slot includes padding for small types.
// Heap values contain the complete packed value, including multivalue offsets.
type rawNamedProperty struct {
	ID         uint16 `json:"property_id"`
	Type       uint16 `json:"property_type"`
	Storage    string `json:"storage"`
	Value      []byte `json:"value_base64"`
	ObjectData []byte `json:"object_data_base64,omitempty"`
}

// These original streams retain mapping information that go-pst's decoded
// NameToIDMap drops, including unknown GUIDs and string-name property sets.
type rawNameIDStreams struct {
	GUID   []byte `json:"guid_stream_base64"`
	Entry  []byte `json:"entry_stream_base64"`
	String []byte `json:"string_stream_base64"`
}

func preserveNamedProperties(msg *pstlib.Message) ([]rawNamedProperty, *rawNameIDStreams, error) {
	var result []rawNamedProperty
	total := 0
	for _, prop := range msg.PropertyContext.Properties {
		if prop.ID < 0x8000 {
			continue
		}
		item := rawNamedProperty{ID: prop.ID, Type: uint16(prop.Type)}
		var err error
		switch {
		case len(prop.Data) != 0:
			item.Storage, item.Value = "inline", prop.Data
		case prop.HNID == 0:
			item.Storage, item.Value = "empty", []byte{}
		default:
			item.Storage = "heap_or_subnode"
			if msg.File == nil || msg.PropertyContext.HeapOnNode == nil || msg.PropertyContext.HeapOnNode.Reader == nil {
				return nil, nil, fmt.Errorf("property %#x has no source reader", prop.ID)
			}
			item.Value, err = bodyPropertyBytes(msg, prop)
			if err != nil {
				return nil, nil, fmt.Errorf("read property %#x: %w", prop.ID, err)
			}
		}
		total += len(item.Value)
		if total > maxPSTBodyBytes {
			return nil, nil, fmt.Errorf("named properties exceed %d bytes", maxPSTBodyBytes)
		}
		if prop.Type == pstlib.PropertyTypeObject && len(item.Value) != 0 {
			// MS-PST section 2.3.3.4 stores PtypObject as an eight-byte
			// subnode identifier and size. The descriptor alone is not the
			// object, so retain the referenced data as well.
			if len(item.Value) != 8 || msg.File == nil || msg.PropertyContext.HeapOnNode == nil || msg.PropertyContext.HeapOnNode.Reader == nil {
				return nil, nil, fmt.Errorf("property %#x has invalid object reference", prop.ID)
			}
			id := pstlib.Identifier(binary.LittleEndian.Uint32(item.Value[:4]))
			size := binary.LittleEndian.Uint32(item.Value[4:])
			if int64(size) > int64(maxPSTBodyBytes-total) {
				return nil, nil, fmt.Errorf("object property %#x exceeds size limit", prop.ID)
			}
			reader, err := msg.File.GetHeapOnNodeReaderFromHNID(id, *msg.PropertyContext.HeapOnNode.Reader, msg.LocalDescriptors...)
			if err != nil {
				return nil, nil, fmt.Errorf("open object property %#x: %w", prop.ID, err)
			}
			if reader.Size() != int64(size) {
				return nil, nil, fmt.Errorf("object property %#x size mismatch", prop.ID)
			}
			item.ObjectData, err = readBoundedRaw(reader, int64(maxPSTBodyBytes-total))
			if err != nil {
				return nil, nil, fmt.Errorf("read object property %#x: %w", prop.ID, err)
			}
			total += len(item.ObjectData)
		}
		result = append(result, item)
	}
	if len(result) == 0 || msg.File == nil || msg.File.Reader == nil {
		return result, nil, nil
	}
	streams, err := readNameIDStreams(msg.File, int64(maxPSTBodyBytes-total))
	return result, streams, err
}

func readBoundedRaw(reader *pstlib.HeapOnNodeReader, remaining int64) ([]byte, error) {
	if reader == nil || reader.Size() < 0 || reader.Size() > remaining {
		return nil, errors.New("invalid or oversized data")
	}
	data := make([]byte, int(reader.Size()))
	if len(data) != 0 {
		n, err := reader.ReadAt(data, 0)
		if n != len(data) && err == nil {
			err = io.ErrUnexpectedEOF
		}
		if err != nil && (!errors.Is(err, io.EOF) || n != len(data)) {
			return nil, fmt.Errorf("read %d of %d bytes: %w", n, len(data), err)
		}
	}
	return data, nil
}

func readNameIDStreams(file *pstlib.File, remaining int64) (*rawNameIDStreams, error) {
	node, err := file.GetNodeBTreeNode(pstlib.IdentifierNameToIDMap)
	if err != nil {
		return nil, fmt.Errorf("find name-to-ID map: %w", err)
	}
	locals, err := file.GetLocalDescriptors(node)
	if err != nil {
		return nil, fmt.Errorf("read name-to-ID descriptors: %w", err)
	}
	block, err := file.GetBlockBTreeNode(node.DataIdentifier)
	if err != nil {
		return nil, fmt.Errorf("find name-to-ID data: %w", err)
	}
	heap, err := file.GetHeapOnNode(block)
	if err != nil {
		return nil, fmt.Errorf("read name-to-ID heap: %w", err)
	}
	ctx, err := file.GetPropertyContext(heap)
	if err != nil {
		return nil, fmt.Errorf("read name-to-ID properties: %w", err)
	}
	streams := &rawNameIDStreams{}
	for _, item := range []struct {
		id  uint16
		dst *[]byte
	}{{2, &streams.GUID}, {3, &streams.Entry}, {4, &streams.String}} {
		r, err := ctx.GetPropertyReader(item.id, locals)
		if err != nil {
			return nil, fmt.Errorf("open name-to-ID stream %d: %w", item.id, err)
		}
		data, err := readBoundedRaw(r.HeapOnNodeReader, remaining)
		if err != nil {
			return nil, fmt.Errorf("read name-to-ID stream %d: %w", item.id, err)
		}
		*item.dst = data
		remaining -= int64(len(data))
	}
	return streams, nil
}

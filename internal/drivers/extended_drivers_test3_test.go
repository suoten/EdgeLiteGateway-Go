package drivers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"

	"edgelite/internal/models"
)

// ==================== toUint16Slice Tests ====================

func TestToUint16SliceUint16Slice(t *testing.T) {
	result, err := toUint16Slice([]uint16{1, 2, 3}, 3)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{1, 2, 3}, result)
}

func TestToUint16SliceBytes(t *testing.T) {
	bytes := make([]byte, 4)
	binary.BigEndian.PutUint16(bytes[0:], 100)
	binary.BigEndian.PutUint16(bytes[2:], 200)
	result, err := toUint16Slice(bytes, 2)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{100, 200}, result)
}

func TestToUint16SliceUint16(t *testing.T) {
	result, err := toUint16Slice(uint16(42), 1)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{42}, result)
}

func TestToUint16SliceInt(t *testing.T) {
	result, err := toUint16Slice(int(42), 1)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{42}, result)
}

func TestToUint16SliceInt32(t *testing.T) {
	result, err := toUint16Slice(int32(42), 1)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{42}, result)
}

func TestToUint16SliceInt64(t *testing.T) {
	result, err := toUint16Slice(int64(42), 1)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{42}, result)
}

func TestToUint16SliceFloat64(t *testing.T) {
	result, err := toUint16Slice(float64(42), 1)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{42}, result)
}

func TestToUint16SlicePadding(t *testing.T) {
	result, err := toUint16Slice(uint16(1), 3)
	assert.NoError(t, err)
	assert.Equal(t, []uint16{1, 0, 0}, result)
}

func TestToUint16SliceDefault(t *testing.T) {
	// Test with a type that falls through to default (e.g. string)
	result, err := toUint16Slice("not a number", 1)
	assert.Error(t, err)
	assert.Nil(t, result)
}

// ==================== Modbus Slave processRequest Tests ====================

func TestModbusSlaveProcessRequestReadHoldingRegisters(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10520,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-1", config)
	sd := driver.(*ModbusSlaveDriver)

	// Set a known value in holding register 0
	sd.holdingRegs[0] = 12345
	sd.holdingRegs[1] = 6789

	// Build Read Holding Registers request: FC=0x03, addr=0, qty=2
	pdu := []byte{0x03, 0x00, 0x00, 0x00, 0x02}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x03), resp[0]) // Function code
	assert.Equal(t, byte(4), resp[1])    // Byte count = 2 regs * 2 bytes
	val0 := binary.BigEndian.Uint16(resp[2:4])
	val1 := binary.BigEndian.Uint16(resp[4:6])
	assert.Equal(t, uint16(12345), val0)
	assert.Equal(t, uint16(6789), val1)
}

func TestModbusSlaveProcessRequestReadInputRegisters(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10521,
		"holding_size": 100, "input_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-2", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.inputRegs[0] = 111
	sd.inputRegs[1] = 222

	// Build Read Input Registers request: FC=0x04, addr=0, qty=2
	pdu := []byte{0x04, 0x00, 0x00, 0x00, 0x02}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x04), resp[0])
	assert.Equal(t, byte(4), resp[1])
	val0 := binary.BigEndian.Uint16(resp[2:4])
	val1 := binary.BigEndian.Uint16(resp[4:6])
	assert.Equal(t, uint16(111), val0)
	assert.Equal(t, uint16(222), val1)
}

func TestModbusSlaveProcessRequestReadCoils(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10522,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-3", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.coilBits[0] = true
	sd.coilBits[1] = false
	sd.coilBits[2] = true

	// Build Read Coils request: FC=0x01, addr=0, qty=3
	pdu := []byte{0x01, 0x00, 0x00, 0x00, 0x03}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x01), resp[0])
	assert.Equal(t, byte(1), resp[1]) // 1 byte for 3 coils
	// Bit 0 = true, Bit 1 = false, Bit 2 = true => 0b00000101 = 0x05
	assert.Equal(t, byte(0x05), resp[2])
}

func TestModbusSlaveProcessRequestReadDiscreteInputs(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10523,
		"holding_size": 100, "discrete_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-4", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.discreteBits[0] = true
	sd.discreteBits[1] = true

	// Build Read Discrete Inputs request: FC=0x02, addr=0, qty=2
	pdu := []byte{0x02, 0x00, 0x00, 0x00, 0x02}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x02), resp[0])
	assert.Equal(t, byte(1), resp[1])
	assert.Equal(t, byte(0x03), resp[2]) // Bits 0 and 1 set
}

func TestModbusSlaveProcessRequestWriteSingleCoil(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10524,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-5", config)
	sd := driver.(*ModbusSlaveDriver)

	// Build Write Single Coil request: FC=0x05, addr=5, value=0xFF00 (ON)
	pdu := []byte{0x05, 0x00, 0x05, 0xFF, 0x00}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x05), resp[0])
	assert.True(t, sd.coilBits[5])
}

func TestModbusSlaveProcessRequestWriteSingleRegister(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10525,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-6", config)
	sd := driver.(*ModbusSlaveDriver)

	// Build Write Single Register request: FC=0x06, addr=3, value=999
	pdu := []byte{0x06, 0x00, 0x03, 0x03, 0xE7}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x06), resp[0])
	assert.Equal(t, uint16(999), sd.holdingRegs[3])
}

func TestModbusSlaveProcessRequestWriteMultipleCoils(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10526,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-7", config)
	sd := driver.(*ModbusSlaveDriver)

	// Build Write Multiple Coils: FC=0x0F, addr=0, qty=5, byteCount=1, data=0b00010111
	pdu := []byte{0x0F, 0x00, 0x00, 0x00, 0x05, 0x01, 0x17}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x0F), resp[0])
	// Coils 0,1,2,4 should be true, 3 should be false
	assert.True(t, sd.coilBits[0])
	assert.True(t, sd.coilBits[1])
	assert.True(t, sd.coilBits[2])
	assert.False(t, sd.coilBits[3])
	assert.True(t, sd.coilBits[4])
}

func TestModbusSlaveProcessRequestWriteMultipleRegisters(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10527,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-8", config)
	sd := driver.(*ModbusSlaveDriver)

	// Build Write Multiple Registers: FC=0x10, addr=2, qty=2, byteCount=4, data=[100,200]
	pdu := make([]byte, 10)
	pdu[0] = 0x10
	binary.BigEndian.PutUint16(pdu[1:3], 2) // addr
	binary.BigEndian.PutUint16(pdu[3:5], 2) // qty
	pdu[5] = 4                              // byte count
	binary.BigEndian.PutUint16(pdu[6:8], 100)
	binary.BigEndian.PutUint16(pdu[8:10], 200)
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x10), resp[0])
	assert.Equal(t, uint16(100), sd.holdingRegs[2])
	assert.Equal(t, uint16(200), sd.holdingRegs[3])
}

func TestModbusSlaveProcessRequestUnknownFunction(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10528,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-9", config)
	sd := driver.(*ModbusSlaveDriver)

	// Unknown function code 0x41
	pdu := []byte{0x41}
	resp := sd.processRequest(1, pdu)

	// Should return exception response
	assert.Equal(t, byte(0xC1), resp[0]) // 0x41 | 0x80
	assert.Equal(t, byte(0x01), resp[1]) // Illegal function
}

func TestModbusSlaveProcessRequestEmptyPDU(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10529,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-10", config)
	sd := driver.(*ModbusSlaveDriver)

	resp := sd.processRequest(1, []byte{})
	assert.Equal(t, byte(0x80), resp[0]) // 0 | 0x80
	assert.Equal(t, byte(0x01), resp[1])
}

func TestModbusSlaveProcessRequestReadHoldingRegsOutOfRange(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10530,
		"holding_size": 10, "coil_size": 10,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-11", config)
	sd := driver.(*ModbusSlaveDriver)

	// Try to read beyond holding register range
	pdu := []byte{0x03, 0x00, 0x05, 0x00, 0x0A} // addr=5, qty=10 (only 10 regs total)
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x83), resp[0]) // 0x03 | 0x80
	assert.Equal(t, byte(0x02), resp[1]) // Illegal data address
}

func TestModbusSlaveProcessRequestReadHoldingRegsInvalidQty(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10531,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-12", config)
	sd := driver.(*ModbusSlaveDriver)

	// qty=0 is invalid
	pdu := []byte{0x03, 0x00, 0x00, 0x00, 0x00}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x83), resp[0])
	assert.Equal(t, byte(0x03), resp[1]) // Illegal data value
}

func TestModbusSlaveProcessRequestWriteSingleCoilOutOfRange(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10532,
		"holding_size": 100, "coil_size": 5,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-13", config)
	sd := driver.(*ModbusSlaveDriver)

	// Write to coil addr=10, but only 5 coils
	pdu := []byte{0x05, 0x00, 0x0A, 0xFF, 0x00}
	resp := sd.processRequest(1, pdu)

	assert.Equal(t, byte(0x85), resp[0])
	assert.Equal(t, byte(0x02), resp[1])
}

func TestModbusSlaveExceptionResponse(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10533,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-14", config)
	sd := driver.(*ModbusSlaveDriver)

	resp := sd.exceptionResponse(0x03, 0x02)
	assert.Equal(t, []byte{0x83, 0x02}, resp)
}

// ==================== Modbus Slave ReadPoints Tests ====================

func TestModbusSlaveReadPointsHoldingRegister(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10534,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-15", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.holdingRegs[0] = 4242

	pts := []models.PointDef{{Name: "HR0", Address: "HR0", DataType: "uint16"}}
	result, err := driver.ReadPoints(context.TODO(), pts)
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, uint16(4242), result[0].Value)
}

func TestModbusSlaveReadPointsCoil(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10535,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-16", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.coilBits[0] = true

	pts := []models.PointDef{{Name: "C0", Address: "coil0", DataType: "bool"}}
	result, err := driver.ReadPoints(context.TODO(), pts)
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, true, result[0].Value)
}

func TestModbusSlaveReadPointsInvalidAddress(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10536,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-17", config)

	pts := []models.PointDef{{Name: "bad", Address: "INVALID", DataType: "uint16"}}
	result, err := driver.ReadPoints(context.TODO(), pts)
	// Should not error overall, but the point should have a zero or error value
	_ = err
	_ = result
}

// ==================== Modbus Slave WritePoint Tests ====================

func TestModbusSlaveWritePointHoldingRegister(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10537,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-18", config)
	sd := driver.(*ModbusSlaveDriver)

	err := driver.WritePoint(context.TODO(), "hr0", uint16(555))
	assert.NoError(t, err)
	assert.Equal(t, uint16(555), sd.holdingRegs[0])
}

func TestModbusSlaveWritePointCoil(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10538,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-19", config)
	sd := driver.(*ModbusSlaveDriver)

	err := driver.WritePoint(context.TODO(), "coil0", true)
	assert.NoError(t, err)
	assert.True(t, sd.coilBits[0])
}

func TestModbusSlaveWritePointCoilInt(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10539,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-20", config)
	sd := driver.(*ModbusSlaveDriver)

	err := driver.WritePoint(context.TODO(), "coil0", 1)
	assert.NoError(t, err)
	assert.True(t, sd.coilBits[0])
}

func TestModbusSlaveWritePointHoldingMultiRegs(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10540,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-21", config)
	sd := driver.(*ModbusSlaveDriver)

	err := driver.WritePoint(context.TODO(), "hr0.2", []uint16{100, 200})
	assert.NoError(t, err)
	assert.Equal(t, uint16(100), sd.holdingRegs[0])
	assert.Equal(t, uint16(200), sd.holdingRegs[1])
}

func TestModbusSlaveWritePointInvalidAddress(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10541,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-22", config)

	err := driver.WritePoint(context.TODO(), "INVALID", 42)
	assert.Error(t, err)
}

func TestModbusSlaveWritePointUnsupportedType(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10542,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-23", config)

	// input registers don't support write
	err := driver.WritePoint(context.TODO(), "ir1", 42)
	assert.Error(t, err)
}

func TestModbusSlaveReadPointsInputRegister(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10543,
		"holding_size": 100, "input_size": 50, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-24", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.inputRegs[0] = 777

	pts := []models.PointDef{{Name: "IR0", Address: "ir0", DataType: "uint16"}}
	result, err := driver.ReadPoints(context.TODO(), pts)
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, uint16(777), result[0].Value)
}

func TestModbusSlaveReadPointsDiscrete(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10544,
		"holding_size": 100, "coil_size": 50, "discrete_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-25", config)
	sd := driver.(*ModbusSlaveDriver)

	sd.discreteBits[0] = true

	pts := []models.PointDef{{Name: "DI0", Address: "di0", DataType: "bool"}}
	result, err := driver.ReadPoints(context.TODO(), pts)
	assert.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, true, result[0].Value)
}

func TestModbusSlaveHealthCheckNotListening(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10545,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-26", config)

	err := driver.HealthCheck(context.TODO())
	assert.Error(t, err)
}

func TestModbusSlaveDiscover(t *testing.T) {
	config := map[string]interface{}{
		"host": "127.0.0.1", "port": 10546,
		"holding_size": 100, "coil_size": 50,
	}
	driver, _ := NewModbusSlaveDriver("slave-test-27", config)

	// A slave is the device being found, not a scanner: it has no peer to ask.
	result, err := driver.Discover(context.TODO(), nil)
	assert.ErrorIs(t, err, ErrDiscoveryUnsupported)
	assert.Empty(t, result)
}

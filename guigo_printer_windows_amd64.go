// Copyright 2026 肖其顿 (XIAO QI DUN)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package guigo

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// winspool 按需加载Windows打印队列及驱动接口
var winspool = syscall.NewLazyDLL("winspool.drv")

// printGDI 按需加载设备纸面及可打印区域查询接口
var printGDI = syscall.NewLazyDLL("gdi32.dll")

// printerInfo4 对应快速枚举打印队列的PRINTER_INFO_4W
type printerInfo4 struct {
	Name, Server *uint16
	Attributes   uint32
}

// windowsDefaultPrinter 获取当前用户默认队列名称
// 返回: string 默认队列名称，查询失败或未设置时为空
func windowsDefaultPrinter() string {
	var size uint32
	proc := winspool.NewProc("GetDefaultPrinterW")
	proc.Call(0, uintptr(unsafe.Pointer(&size)))
	if size == 0 || size > 32768 {
		return ""
	}
	buffer := make([]uint16, size)
	ok, _, _ := proc.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(buffer)
}

// windowsWideString 复制系统提供的UTF16字符串
// 入参: value 系统UTF16字符串指针
// 返回: string 字符串副本
func windowsWideString(value *uint16) string {
	if value == nil {
		return ""
	}
	var buffer []uint16
	for i := 0; i < 32768; i++ {
		v := *(*uint16)(unsafe.Add(unsafe.Pointer(value), i*2))
		if v == 0 {
			break
		}
		buffer = append(buffer, v)
	}
	return syscall.UTF16ToString(buffer)
}

// windowsPrintMode 取得驱动完整DEVMODE，不改变系统默认值
// 入参: name 系统队列名称
// 返回: []byte 完整DEVMODE数据, uintptr 待关闭打印机句柄, error 驱动读取错误
func windowsPrintMode(name string) ([]byte, uintptr, error) {
	var printer uintptr
	ok, _, err := winspool.NewProc("OpenPrinterW").Call(uintptr(unsafe.Pointer(wide(name))), uintptr(unsafe.Pointer(&printer)), 0)
	if ok == 0 {
		return nil, 0, err
	}
	proc := winspool.NewProc("DocumentPropertiesW")
	size, _, _ := proc.Call(0, printer, uintptr(unsafe.Pointer(wide(name))), 0, 0, 0)
	if int32(size) < 220 || size > 1<<20 {
		winspool.NewProc("ClosePrinter").Call(printer)
		return nil, 0, fmt.Errorf("invalid printer settings size")
	}
	data := make([]byte, size)
	status, _, _ := proc.Call(0, printer, uintptr(unsafe.Pointer(wide(name))), uintptr(unsafe.Pointer(&data[0])), 0, 2)
	if int32(status) != 1 {
		winspool.NewProc("ClosePrinter").Call(printer)
		return nil, 0, fmt.Errorf("read printer settings")
	}
	return data, printer, nil
}

// listPrinters 枚举已安装本地和连接队列，不连接远端设备读取详细驱动数据
// 入参: ctx 查询上下文
// 返回: []PrinterInfo 系统打印队列, error 查询或取消错误
func (n *nativeWindow) listPrinters(ctx context.Context) ([]PrinterInfo, error) {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var size, count uint32
	proc := winspool.NewProc("EnumPrintersW")
	proc.Call(6, 0, 4, 0, 0, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)))
	if size == 0 {
		return []PrinterInfo{}, ctx.Err()
	}
	if size > 64<<20 {
		return nil, fmt.Errorf("printer list too large")
	}
	buffer := make([]byte, size)
	ok, _, err := proc.Call(6, 0, 4, uintptr(unsafe.Pointer(&buffer[0])), uintptr(size), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)))
	if ok == 0 {
		return nil, err
	}
	if uint64(count)*uint64(unsafe.Sizeof(printerInfo4{})) > uint64(len(buffer)) {
		return nil, fmt.Errorf("invalid printer list")
	}
	defaultName := windowsDefaultPrinter()
	result := make([]PrinterInfo, 0, count)
	for _, item := range unsafe.Slice((*printerInfo4)(unsafe.Pointer(&buffer[0])), count) {
		name := windowsWideString(item.Name)
		result = append(result, PrinterInfo{Name: name, Default: name == defaultName})
	}
	runtime.KeepAlive(buffer)
	return result, ctx.Err()
}

// queryPrinter 查询当前纸张、方向对应的驱动可打印区域，毫米换算使用设备DPI
// 入参: ctx 查询上下文, o 待核对设备设置
// 返回: PrinterProfile 设备能力及当前纸面, error 配置、查询或取消错误
func (n *nativeWindow) queryPrinter(ctx context.Context, o PrintOptions) (PrinterProfile, error) {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return PrinterProfile{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return PrinterProfile{}, err
	}
	name := o.Printer
	if name == "" {
		name = windowsDefaultPrinter()
	}
	if name == "" {
		return PrinterProfile{}, fmt.Errorf("no default printer")
	}
	data, printer, err := windowsPrintMode(name)
	if err != nil {
		return PrinterProfile{}, err
	}
	defer winspool.NewProc("ClosePrinter").Call(printer)
	get := func(offset int) int { return int(int16(binary.LittleEndian.Uint16(data[offset:]))) }
	set := func(offset, value int) { binary.LittleEndian.PutUint16(data[offset:], uint16(value)) }
	capability := func(code int, output unsafe.Pointer) int {
		value, _, _ := winspool.NewProc("DeviceCapabilitiesW").Call(uintptr(unsafe.Pointer(wide(name))), 0, uintptr(code), uintptr(output), uintptr(unsafe.Pointer(&data[0])))
		return int(int32(value))
	}
	p := PrinterProfile{Printer: PrinterInfo{Name: name, Default: name == windowsDefaultPrinter()}, Papers: []PrintPaper{}, Color: capability(32, nil) == 1, Duplex: capability(7, nil) == 1, MaxCopies: max(1, capability(18, nil)), Properties: true}
	count := capability(2, nil)
	if count > 0 && count < 4096 {
		ids := make([]uint16, count)
		sizes := make([]point, count)
		names := make([]uint16, count*64)
		if capability(2, unsafe.Pointer(&ids[0])) != count || capability(3, unsafe.Pointer(&sizes[0])) != count || capability(16, unsafe.Pointer(&names[0])) != count {
			return p, fmt.Errorf("printer paper capabilities changed")
		}
		for i, id := range ids {
			if sizes[i].X > 0 && sizes[i].Y > 0 {
				p.Papers = append(p.Papers, PrintPaper{ID: strconv.Itoa(int(id)), Name: syscall.UTF16ToString(names[i*64 : (i+1)*64]), Width: float64(sizes[i].X) / 10, Height: float64(sizes[i].Y) / 10})
			}
		}
	}
	minimum, maximum := capability(4, nil), capability(5, nil)
	p.CustomPaper = minimum > 0 && maximum > 0 && minimum != maximum
	if o.Width == 0 {
		for _, paper := range p.Papers {
			if paper.ID == strconv.Itoa(get(78)) {
				o.Width, o.Height = paper.Width, paper.Height
				break
			}
		}
		if o.Width == 0 {
			o.Width, o.Height = float64(get(82))/10, float64(get(80))/10
		}
	}
	if o.Width <= 0 || o.Height <= 0 {
		return p, fmt.Errorf("printer has no usable default paper")
	}
	paperID := 0
	for _, paper := range p.Papers {
		if math.Abs(paper.Width-o.Width) < 0.2 && math.Abs(paper.Height-o.Height) < 0.2 {
			paperID, _ = strconv.Atoi(paper.ID)
			break
		}
	}
	if paperID == 0 {
		if !p.CustomPaper || o.Width > 3276 || o.Height > 3276 {
			return p, fmt.Errorf("%w: paper size", ErrPrintUnsupported)
		}
		if o.Width < float64(uint32(minimum)&65535)/10 || o.Height < float64(uint32(minimum)>>16)/10 || o.Width > float64(uint32(maximum)&65535)/10 || o.Height > float64(uint32(maximum)>>16)/10 {
			return p, fmt.Errorf("%w: custom paper size", ErrPrintUnsupported)
		}
	}
	if o.Orientation == "" {
		o.Orientation = "portrait"
		if get(76) == 2 {
			o.Orientation = "landscape"
		}
	}
	if o.Copies == 0 {
		o.Copies = max(1, get(86))
	}
	requestedDuplex := o.Duplex
	if o.Duplex == "" {
		if p.Duplex {
			o.Duplex = map[int]string{2: "longEdge", 3: "shortEdge"}[get(94)]
		}
		if o.Duplex == "" {
			o.Duplex = "simplex"
		}
	}
	if o.Color == "" {
		o.Color = "grayscale"
		if p.Color && get(92) == 2 {
			o.Color = "color"
		}
	}
	if o.Collation == "" {
		o.Collation = "uncollated"
		if get(100) == 1 {
			o.Collation = "collated"
		}
	}
	o.Printer = name
	setDuplex := p.Duplex && (requestedDuplex != "" || o.Duplex != "simplex")
	fields := (binary.LittleEndian.Uint32(data[72:]) | 1 | 2 | 0x10 | 0x100 | 0x800 | 0x8000) &^ 0x10000
	set(78, paperID)
	if paperID == 0 {
		fields |= 4 | 8
		set(80, int(math.Round(o.Height*10)))
		set(82, int(math.Round(o.Width*10)))
	} else {
		fields &^= 4 | 8
	}
	set(76, 1)
	if o.Orientation == "landscape" {
		set(76, 2)
	}
	set(84, 100)
	set(86, o.Copies)
	if setDuplex {
		fields |= 0x1000
		set(94, map[string]int{"simplex": 1, "longEdge": 2, "shortEdge": 3}[o.Duplex])
	}
	set(92, 1)
	if o.Color == "color" {
		set(92, 2)
	}
	set(100, 0)
	if o.Collation == "collated" {
		set(100, 1)
	}
	binary.LittleEndian.PutUint32(data[72:], fields)
	p.Options = o
	p.Width, p.Height = o.Width, o.Height
	if o.Orientation == "landscape" {
		p.Width, p.Height = p.Height, p.Width
	}
	if o.Color == "color" && !p.Color || o.Duplex != "simplex" && !p.Duplex || o.Copies > p.MaxCopies {
		return p, fmt.Errorf("%w: device settings", ErrPrintUnsupported)
	}
	status, _, _ := winspool.NewProc("DocumentPropertiesW").Call(0, printer, uintptr(unsafe.Pointer(wide(name))), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&data[0])), 10)
	if int32(status) != 1 {
		return p, fmt.Errorf("validate printer settings")
	}
	if get(76) != (map[string]int{"portrait": 1, "landscape": 2}[o.Orientation]) || setDuplex && get(94) != (map[string]int{"simplex": 1, "longEdge": 2, "shortEdge": 3}[o.Duplex]) {
		return p, fmt.Errorf("%w: driver changed device settings", ErrPrintUnsupported)
	}
	dc, _, err := printGDI.NewProc("CreateICW").Call(uintptr(unsafe.Pointer(wide("WINSPOOL"))), uintptr(unsafe.Pointer(wide(name))), 0, uintptr(unsafe.Pointer(&data[0])))
	if dc == 0 {
		return p, err
	}
	defer printGDI.NewProc("DeleteDC").Call(dc)
	device := func(index int) float64 {
		value, _, _ := printGDI.NewProc("GetDeviceCaps").Call(dc, uintptr(index))
		return float64(int32(value))
	}
	dx, dy := device(88), device(90)
	if dx <= 0 || dy <= 0 || math.Abs(device(110)*25.4/dx-p.Width) > 1 || math.Abs(device(111)*25.4/dy-p.Height) > 1 {
		return p, fmt.Errorf("%w: driver changed paper size", ErrPrintUnsupported)
	}
	x, y := math.Max(0, device(112)*25.4/dx), math.Max(0, device(113)*25.4/dy)
	p.PrintableArea = PrintRectangle{X: x, Y: y, Width: math.Min(p.Width-x, device(8)*25.4/dx), Height: math.Min(p.Height-y, device(10)*25.4/dy)}
	runtime.KeepAlive(data)
	return p, ctx.Err()
}

// showPrinterProperties 在STA最外层消息循环打开系统设备属性
// 入参: ctx 调用上下文, name 系统队列名称
// 返回: error 不支持、取消或属性窗口错误
func (n *nativeWindow) showPrinterProperties(ctx context.Context, name string) error {
	return n.invokeTop(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var printer uintptr
		ok, _, err := winspool.NewProc("OpenPrinterW").Call(uintptr(unsafe.Pointer(wide(name))), uintptr(unsafe.Pointer(&printer)), 0)
		if ok == 0 {
			return err
		}
		defer winspool.NewProc("ClosePrinter").Call(printer)
		ok, _, err = winspool.NewProc("PrinterProperties").Call(n.hwnd, printer)
		if ok == 0 {
			if err == syscall.Errno(0) {
				return ErrCanceled
			}
			return err
		}
		return ctx.Err()
	})
}

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

//go:build !android && (amd64 || arm64)

package guigo

import (
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var gtk linuxAPI
var linuxLoadMu sync.Mutex
var linuxLoadedPath string
var linuxLoaded bool

// linuxError 对应GLib错误结构的原生布局
type linuxError struct {
	Domain  uint32
	Code    int32
	Message *byte
}

// linuxList 对应GLib单向链表的原生布局
type linuxList struct {
	Data uintptr
	Next *linuxList
}

// nativeValue 对应GObject属性值的原生布局
type nativeValue struct {
	Type uintptr
	Data [2]uintptr
}

// linuxRectangle 对应GTK分配区域的原生布局
type linuxRectangle struct {
	X, Y, Width, Height int32
}

// linuxAPI 保存动态加载的原生入口
type linuxAPI struct {
	PrintNew         func(uintptr) uintptr                                                     `symbol:"webkit_print_operation_new"`
	PrintDialog      func(uintptr, uintptr) int32                                              `symbol:"webkit_print_operation_run_dialog"`
	PrintRun         func(uintptr)                                                             `symbol:"webkit_print_operation_print"`
	PrintSettings    func(uintptr, uintptr)                                                    `symbol:"webkit_print_operation_set_print_settings"`
	PrintPageSetup   func(uintptr, uintptr)                                                    `symbol:"webkit_print_operation_set_page_setup"`
	PrintSettingsNew func() uintptr                                                            `symbol:"gtk_print_settings_new"`
	PrintSet         func(uintptr, string, string)                                             `symbol:"gtk_print_settings_set"`
	PrintRanges      func(uintptr, unsafe.Pointer, int32)                                      `symbol:"gtk_print_settings_set_page_ranges"`
	PageSetupNew     func() uintptr                                                            `symbol:"gtk_page_setup_new"`
	PaperNew         func(string, string, float64, float64, int32) uintptr                     `symbol:"gtk_paper_size_new_custom"`
	PaperFree        func(uintptr)                                                             `symbol:"gtk_paper_size_free"`
	PagePaper        func(uintptr, uintptr)                                                    `symbol:"gtk_page_setup_set_paper_size"`
	PageOrientation  func(uintptr, int32)                                                      `symbol:"gtk_page_setup_set_orientation"`
	PageTop          func(uintptr, float64, int32)                                             `symbol:"gtk_page_setup_set_top_margin"`
	PageBottom       func(uintptr, float64, int32)                                             `symbol:"gtk_page_setup_set_bottom_margin"`
	PageLeft         func(uintptr, float64, int32)                                             `symbol:"gtk_page_setup_set_left_margin"`
	PageRight        func(uintptr, float64, int32)                                             `symbol:"gtk_page_setup_set_right_margin"`
	SignalDisconnect func(uintptr, uint64)                                                     `symbol:"g_signal_handler_disconnect"`
	ScaleFactor      func(uintptr) int32                                                       `symbol:"gtk_widget_get_scale_factor"`
	RequestView      func(uintptr) uintptr                                                     `symbol:"webkit_uri_scheme_request_get_web_view"`
	Prgname          func(*byte)                                                               `symbol:"g_set_prgname"`
	GetPrgname       func() *byte                                                              `symbol:"g_get_prgname"`
	ValueInit        func(*nativeValue, uintptr) *nativeValue                                  `symbol:"g_value_init"`
	ValueObject      func(*nativeValue, uintptr)                                               `symbol:"g_value_set_object"`
	ValueUnset       func(*nativeValue)                                                        `symbol:"g_value_unset"`
	Idle             func(int32, uintptr, uintptr, uintptr) uint32                             `symbol:"g_idle_add_full"`
	DialogErrorQuark func() uint32                                                             `symbol:"gtk_dialog_error_quark"`
	IOErrorQuark     func() uint32                                                             `symbol:"g_io_error_quark"`
	LaunchURI        func(string, uintptr, **linuxError) bool                                  `symbol:"g_app_info_launch_default_for_uri"`
	UnixInput        func(int32, bool) uintptr                                                 `symbol:"g_unix_input_stream_new"`
	Init             func() bool                                                               `symbol:"gtk_init_check"`
	Major            func() uint32                                                             `symbol:"gtk_get_major_version"`
	Minor            func() uint32                                                             `symbol:"gtk_get_minor_version"`
	WindowNew        func() uintptr                                                            `symbol:"gtk_window_new"`
	Title            func(uintptr, string)                                                     `symbol:"gtk_window_set_title"`
	Size             func(uintptr, int32, int32)                                               `symbol:"gtk_window_set_default_size"`
	Minimum          func(uintptr, int32, int32)                                               `symbol:"gtk_widget_set_size_request"`
	Resizable        func(uintptr, bool)                                                       `symbol:"gtk_window_set_resizable"`
	Child            func(uintptr, uintptr)                                                    `symbol:"gtk_window_set_child"`
	Titlebar         func(uintptr, uintptr)                                                    `symbol:"gtk_window_set_titlebar"`
	BoxNew           func(int32, int32) uintptr                                                `symbol:"gtk_box_new"`
	Visible          func(uintptr, bool)                                                       `symbol:"gtk_widget_set_visible"`
	OverlayNew       func() uintptr                                                            `symbol:"gtk_overlay_new"`
	OverlayChild     func(uintptr, uintptr)                                                    `symbol:"gtk_overlay_set_child"`
	OverlayAdd       func(uintptr, uintptr)                                                    `symbol:"gtk_overlay_add_overlay"`
	OverlayRemove    func(uintptr, uintptr)                                                    `symbol:"gtk_overlay_remove_overlay"`
	WindowControls   func(int32) uintptr                                                       `symbol:"gtk_window_controls_new"`
	ControlsEmpty    func(uintptr) bool                                                        `symbol:"gtk_window_controls_get_empty"`
	CSSProviderNew   func() uintptr                                                            `symbol:"gtk_css_provider_new"`
	CSSLoad          func(uintptr, string, int64)                                              `symbol:"gtk_css_provider_load_from_data"`
	CSSAdd           func(uintptr, uintptr, uint32)                                            `symbol:"gtk_style_context_add_provider_for_display"`
	CSSRemove        func(uintptr, uintptr)                                                    `symbol:"gtk_style_context_remove_provider_for_display"`
	CSSClass         func(uintptr, string)                                                     `symbol:"gtk_widget_add_css_class"`
	WindowHandle     func() uintptr                                                            `symbol:"gtk_window_handle_new"`
	WidgetWidth      func(uintptr) int32                                                       `symbol:"gtk_widget_get_width"`
	WidgetHeight     func(uintptr) int32                                                       `symbol:"gtk_widget_get_height"`
	Measure          func(uintptr, int32, int32, *int32, *int32, *int32, *int32)               `symbol:"gtk_widget_measure"`
	IsActive         func(uintptr) bool                                                        `symbol:"gtk_window_is_active"`
	IsMaximized      func(uintptr) bool                                                        `symbol:"gtk_window_is_maximized"`
	Present          func(uintptr)                                                             `symbol:"gtk_window_present"`
	Unminimize       func(uintptr)                                                             `symbol:"gtk_window_unminimize"`
	Maximize         func(uintptr)                                                             `symbol:"gtk_window_maximize"`
	Destroy          func(uintptr)                                                             `symbol:"gtk_window_destroy"`
	IconName         func(uintptr, string)                                                     `symbol:"gtk_window_set_icon_name"`
	Display          func(uintptr) uintptr                                                     `symbol:"gtk_widget_get_display"`
	IconTheme        func(uintptr) uintptr                                                     `symbol:"gtk_icon_theme_get_for_display"`
	IconPath         func(uintptr, string)                                                     `symbol:"gtk_icon_theme_add_search_path"`
	Connect          func(uintptr, string, uintptr, uintptr, uintptr, uint32) uint64           `symbol:"g_signal_connect_data"`
	Ref              func(uintptr) uintptr                                                     `symbol:"g_object_ref"`
	Sink             func(uintptr) uintptr                                                     `symbol:"g_object_ref_sink"`
	Unref            func(uintptr)                                                             `symbol:"g_object_unref"`
	ObjectNew        func(uintptr, uint32, **byte, *nativeValue) uintptr                       `symbol:"g_object_new_with_properties"`
	Free             func(unsafe.Pointer)                                                      `symbol:"g_free"`
	ErrorFree        func(*linuxError)                                                         `symbol:"g_error_free"`
	LoopNew          func(uintptr, bool) uintptr                                               `symbol:"g_main_loop_new"`
	LoopRun          func(uintptr)                                                             `symbol:"g_main_loop_run"`
	LoopQuit         func(uintptr)                                                             `symbol:"g_main_loop_quit"`
	LoopUnref        func(uintptr)                                                             `symbol:"g_main_loop_unref"`
	Timeout          func(int32, uint32, uintptr, uintptr, uintptr) uint32                     `symbol:"g_timeout_add_full"`
	SourceRemove     func(uint32) bool                                                         `symbol:"g_source_remove"`
	ContextNew       func() uintptr                                                            `symbol:"webkit_web_context_new"`
	SessionNew       func(string, string) uintptr                                              `symbol:"webkit_network_session_new"`
	ViewType         func() uintptr                                                            `symbol:"webkit_web_view_get_type"`
	ManagerNew       func() uintptr                                                            `symbol:"webkit_user_content_manager_new"`
	RegisterMessage  func(uintptr, string, uintptr) bool                                       `symbol:"webkit_user_content_manager_register_script_message_handler"`
	ScriptNew        func(string, int32, int32, uintptr, uintptr) uintptr                      `symbol:"webkit_user_script_new"`
	AddScript        func(uintptr, uintptr)                                                    `symbol:"webkit_user_content_manager_add_script"`
	ScriptUnref      func(uintptr)                                                             `symbol:"webkit_user_script_unref"`
	Security         func(uintptr) uintptr                                                     `symbol:"webkit_web_context_get_security_manager"`
	SecureScheme     func(uintptr, string)                                                     `symbol:"webkit_security_manager_register_uri_scheme_as_secure"`
	CorsScheme       func(uintptr, string)                                                     `symbol:"webkit_security_manager_register_uri_scheme_as_cors_enabled"`
	RegisterScheme   func(uintptr, string, uintptr, uintptr, uintptr)                          `symbol:"webkit_web_context_register_uri_scheme"`
	Load             func(uintptr, string)                                                     `symbol:"webkit_web_view_load_uri"`
	URI              func(uintptr) string                                                      `symbol:"webkit_web_view_get_uri"`
	Evaluate         func(uintptr, string, int64, uintptr, uintptr, uintptr, uintptr, uintptr) `symbol:"webkit_web_view_evaluate_javascript"`
	EvalFinish       func(uintptr, uintptr, **linuxError) uintptr                              `symbol:"webkit_web_view_evaluate_javascript_finish"`
	TryClose         func(uintptr)                                                             `symbol:"webkit_web_view_try_close"`
	JSText           func(uintptr) *byte                                                       `symbol:"jsc_value_to_string"`
	RequestURI       func(uintptr) string                                                      `symbol:"webkit_uri_scheme_request_get_uri"`
	RequestMethod    func(uintptr) string                                                      `symbol:"webkit_uri_scheme_request_get_http_method"`
	RequestHeaders   func(uintptr) uintptr                                                     `symbol:"webkit_uri_scheme_request_get_http_headers"`
	RequestBody      func(uintptr) uintptr                                                     `symbol:"webkit_uri_scheme_request_get_http_body"`
	ResponseNew      func(uintptr, int64) uintptr                                              `symbol:"webkit_uri_scheme_response_new"`
	ResponseStatus   func(uintptr, uint32, string)                                             `symbol:"webkit_uri_scheme_response_set_status"`
	ResponseType     func(uintptr, string)                                                     `symbol:"webkit_uri_scheme_response_set_content_type"`
	ResponseHeaders  func(uintptr, uintptr)                                                    `symbol:"webkit_uri_scheme_response_set_http_headers"`
	FinishResponse   func(uintptr, uintptr)                                                    `symbol:"webkit_uri_scheme_request_finish_with_response"`
	HeadersNew       func(int32) uintptr                                                       `symbol:"soup_message_headers_new"`
	HeadersAppend    func(uintptr, string, string)                                             `symbol:"soup_message_headers_append"`
	HeadersEach      func(uintptr, uintptr, uintptr)                                           `symbol:"soup_message_headers_foreach"`
	HeadersUnref     func(uintptr)                                                             `symbol:"soup_message_headers_unref"`
	InputRead        func(uintptr, unsafe.Pointer, uintptr, uintptr, **linuxError) int64       `symbol:"g_input_stream_read"`
	FileNew          func(string) uintptr                                                      `symbol:"g_file_new_for_path"`
	FileRead         func(uintptr, uintptr, **linuxError) uintptr                              `symbol:"g_file_read"`
	FilePath         func(uintptr) *byte                                                       `symbol:"g_file_get_path"`
	CancelNew        func() uintptr                                                            `symbol:"g_cancellable_new"`
	Cancel           func(uintptr)                                                             `symbol:"g_cancellable_cancel"`
	DialogNew        func() uintptr                                                            `symbol:"gtk_file_dialog_new"`
	DialogTitle      func(uintptr, string)                                                     `symbol:"gtk_file_dialog_set_title"`
	DialogModal      func(uintptr, bool)                                                       `symbol:"gtk_file_dialog_set_modal"`
	DialogFolder     func(uintptr, uintptr)                                                    `symbol:"gtk_file_dialog_set_initial_folder"`
	DialogFile       func(uintptr, uintptr)                                                    `symbol:"gtk_file_dialog_set_initial_file"`
	DialogName       func(uintptr, string)                                                     `symbol:"gtk_file_dialog_set_initial_name"`
	DialogFilters    func(uintptr, uintptr)                                                    `symbol:"gtk_file_dialog_set_filters"`
	DialogOpen       func(uintptr, uintptr, uintptr, uintptr, uintptr)                         `symbol:"gtk_file_dialog_open"`
	DialogSave       func(uintptr, uintptr, uintptr, uintptr, uintptr)                         `symbol:"gtk_file_dialog_save"`
	DialogDirectory  func(uintptr, uintptr, uintptr, uintptr, uintptr)                         `symbol:"gtk_file_dialog_select_folder"`
	DialogMultiple   func(uintptr, uintptr, uintptr, uintptr, uintptr)                         `symbol:"gtk_file_dialog_open_multiple"`
	OpenFinish       func(uintptr, uintptr, **linuxError) uintptr                              `symbol:"gtk_file_dialog_open_finish"`
	SaveFinish       func(uintptr, uintptr, **linuxError) uintptr                              `symbol:"gtk_file_dialog_save_finish"`
	DirectoryFinish  func(uintptr, uintptr, **linuxError) uintptr                              `symbol:"gtk_file_dialog_select_folder_finish"`
	MultipleFinish   func(uintptr, uintptr, **linuxError) uintptr                              `symbol:"gtk_file_dialog_open_multiple_finish"`
	FilterType       func() uintptr                                                            `symbol:"gtk_file_filter_get_type"`
	FilterNew        func() uintptr                                                            `symbol:"gtk_file_filter_new"`
	FilterName       func(uintptr, string)                                                     `symbol:"gtk_file_filter_set_name"`
	FilterPattern    func(uintptr, string)                                                     `symbol:"gtk_file_filter_add_pattern"`
	ListNew          func(uintptr) uintptr                                                     `symbol:"g_list_store_new"`
	ListAppend       func(uintptr, uintptr)                                                    `symbol:"g_list_store_append"`
	ListCount        func(uintptr) uint32                                                      `symbol:"g_list_model_get_n_items"`
	ListItem         func(uintptr, uint32) uintptr                                             `symbol:"g_list_model_get_item"`
	NavigationAction func(uintptr) uintptr                                                     `symbol:"webkit_navigation_policy_decision_get_navigation_action"`
	ActionRequest    func(uintptr) uintptr                                                     `symbol:"webkit_navigation_action_get_request"`
	NavigationURI    func(uintptr) string                                                      `symbol:"webkit_uri_request_get_uri"`
	PolicyIgnore     func(uintptr)                                                             `symbol:"webkit_policy_decision_ignore"`
	DropNew          func(uintptr, uint32) uintptr                                             `symbol:"gtk_drop_target_new"`
	DropType         func() uintptr                                                            `symbol:"gdk_file_list_get_type"`
	DropFiles        func(uintptr) *linuxList                                                  `symbol:"gdk_file_list_get_files"`
	ListFree         func(*linuxList)                                                          `symbol:"g_slist_free"`
	ValueBoxed       func(uintptr) uintptr                                                     `symbol:"g_value_get_boxed"`
	ControllerPhase  func(uintptr, int32)                                                      `symbol:"gtk_event_controller_set_propagation_phase"`
	AddController    func(uintptr, uintptr)                                                    `symbol:"gtk_widget_add_controller"`
}

// loadLinux 加载GTK4和WebKitGTK6并校验全部入口
// 入参: path WebKitGTK动态库路径
// 返回: error 加载或版本错误
func loadLinux(path string) error {
	linuxLoadMu.Lock()
	defer linuxLoadMu.Unlock()
	if linuxLoaded {
		if path != linuxLoadedPath {
			return fmt.Errorf("native runtime already loaded from a different path")
		}
		return nil
	}
	webkit := path
	if webkit == "" {
		webkit = "libwebkitgtk-6.0.so.4"
	}
	names := []string{"libgtk-4.so.1", webkit, "libgobject-2.0.so.0", "libgio-2.0.so.0", "libglib-2.0.so.0", "libsoup-3.0.so.0", "libjavascriptcoregtk-6.0.so.1"}
	var handles []uintptr
	complete := false
	defer func() {
		if !complete {
			for i := len(handles) - 1; i >= 0; i-- {
				_ = purego.Dlclose(handles[i])
			}
		}
	}()
	for _, name := range names {
		handle, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return fmt.Errorf("load %s: %w", name, err)
		}
		handles = append(handles, handle)
	}
	var api linuxAPI
	value := reflect.ValueOf(&api).Elem()
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Tag.Get("symbol")
		var address uintptr
		for _, handle := range handles {
			address, _ = purego.Dlsym(handle, name)
			if address != 0 {
				break
			}
		}
		if address == 0 {
			return fmt.Errorf("GTK >= 4.10 and WebKitGTK 6.0 required: missing %s", name)
		}
		purego.RegisterFunc(value.Field(i).Addr().Interface(), address)
	}
	if api.Major() != 4 || api.Minor() < 10 {
		return fmt.Errorf("GTK >= 4.10 required")
	}
	gtk = api
	linuxLoadedPath, linuxLoaded = path, true
	complete = true
	return nil
}

// nativeString 复制原生零结尾字符串
// 入参: p 原生字符指针
// 返回: string 字符串副本
func nativeString(p *byte) string {
	if p == nil {
		return ""
	}
	var data []byte
	for offset := uintptr(0); ; offset++ {
		b := *(*byte)(unsafe.Add(unsafe.Pointer(p), offset))
		if b == 0 {
			return string(data)
		}
		data = append(data, b)
	}
}

// nativeError 复制并释放GError
// 入参: p 原生错误指针
// 返回: error 错误副本
func nativeError(p *linuxError) error {
	if p == nil {
		return nil
	}
	defer gtk.ErrorFree(p)
	return fmt.Errorf("%s", nativeString(p.Message))
}

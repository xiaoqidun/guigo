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

//go:build (windows && amd64) || (linux && !android && (amd64 || arm64))

package guigo

// bridgeScript 在文档创建时安装Promise调用桥与事件订阅
const bridgeScript = `(() => {
	if (location.origin !== "` + resourceOrigin + `" || window !== window.top) return;
	let serial = 0;
	let closed = false;
	const pending = new Map();
	const listeners = new Map();
	const uploads = [];
	let uploadBytes = 0;
	let uploadCount = 0;
	const drain = () => {
		while (uploads.length) {
			const next = uploads[0];
			if (uploadCount >= 4 || uploadCount && uploadBytes + next.size > 32 * 1024 * 1024) break;
			uploads.shift();
			next.cleanup();
			if (next.signal?.aborted) { next.reject(next.signal.reason); continue; }
			uploadCount++;
			uploadBytes += next.size;
			next.resolve(() => { uploadCount--; uploadBytes -= next.size; drain(); });
		}
	};
	const reserve = (size, signal) => new Promise((resolve, reject) => {
		const abort = () => {
			const index = uploads.indexOf(item);
			if (index < 0) return;
			uploads.splice(index, 1);
			item.cleanup();
			reject(signal.reason);
			drain();
		};
		const item = {size, signal, resolve, reject, cleanup:() => signal?.removeEventListener("abort", abort)};
		uploads.push(item);
		signal?.addEventListener("abort", abort, {once:true});
		if (signal?.aborted) abort();
		else drain();
	});
	const packet = args => {
		const parts = [];
		const bodies = [];
		const seen = new Set();
		const visit = (value, path, converted = false) => {
			if (value instanceof Blob || value instanceof ArrayBuffer || ArrayBuffer.isView(value)) {
				const body = value instanceof Blob ? value : ArrayBuffer.isView(value) ? new Uint8Array(value.buffer, value.byteOffset, value.byteLength) : new Uint8Array(value);
				parts.push({path, size:body.size ?? body.byteLength});
				bodies.push(body);
				return null;
			}
			if (!value || typeof value !== "object") return value;
			if (!converted && typeof value.toJSON === "function") return visit(value.toJSON(), path, true);
			if (seen.has(value)) throw new TypeError("cyclic arguments");
			seen.add(value);
			let result;
			if (Array.isArray(value)) result = value.map((item, index) => visit(item, [...path, String(index)]));
			else {
				result = Object.create(null);
				for (const [key, item] of Object.entries(value)) result[key] = visit(item, [...path, key]);
			}
			seen.delete(value);
			return result;
		};
		const value = visit(args, []);
		if (!parts.length) return {args:value};
		const header = new TextEncoder().encode(JSON.stringify({args:value, parts}));
		if (header.length > 4 * 1024 * 1024) throw new RangeError("binary header too large");
		const prefix = new Uint8Array(4);
		new DataView(prefix.buffer).setUint32(0, header.length, true);
		return {parts:[prefix, header, ...bodies], size:4 + header.length + parts.reduce((total, part) => total + part.size, 0)};
	};
	const cancel = id => {
		try { chrome.webview.postMessage({type:"cancel", id}); }
		catch {}
	};
	const settle = message => {
		const call = pending.get(message.id);
		if (!call) return;
		pending.delete(message.id);
		call.cleanup();
		try {
			chrome.webview.postMessage({type:message.error ? "cancel" : "accept", id:message.id});
		} catch (error) {
			cancel(message.id);
			call.reject(error);
			return;
		}
		if (message.error) {
			const error = new Error(message.error);
			error.name = message.errorName || "Error";
			if (message.code) error.code = message.code;
			call.reject(error);
		} else call.resolve(message.value);
	};
	chrome.webview.addEventListener("message", event => {
		const message = event.data;
		if (!message || typeof message !== "object") return;
		if (message.type === "reply") settle(message);
		if (message.type === "upload") pending.get(message.id)?.upload();
		if (message.type === "event") {
			for (const listener of [...(listeners.get(message.name) || [])]) {
				try {
					listener(message.value);
				} catch (error) {
					reportError(error);
				}
			}
		}
	});
	Object.defineProperty(window, "guigo", {
		value: Object.freeze({
			async call(name, args = null, options = {}) {
				if (closed) throw new Error("document closed");
				if (typeof name !== "string" || !name) throw new TypeError("method must be a non-empty string");
				const signal = options.signal;
				signal?.throwIfAborted();
				if (pending.size + uploads.length >= 128) throw new Error("too many pending calls");
				const data = packet(args);
				let release;
				if (data.parts) release = await reserve(data.size, signal);
				try {
					if (closed) throw new Error("document closed");
					const body = data.parts ? new Blob(data.parts) : null;
					return await new Promise((resolve, reject) => {
						const id = String(++serial);
						const abort = () => {
							cancel(id);
							const call = pending.get(id);
							pending.delete(id);
							call?.cleanup();
							reject(signal.reason);
						};
						const cleanup = () => signal?.removeEventListener("abort", abort);
						const upload = async () => {
							try {
								const response = await fetch("/__guigo/call?id=" + id, {method:"POST", body, signal, headers:{"X-Guigo-Length":String(data.size)}});
								if (!response.ok) throw new Error("binary call failed");
								settle(await response.json());
							} catch (error) {
								cancel(id);
								pending.delete(id);
								cleanup();
								reject(error);
							}
						};
						pending.set(id, {resolve, reject, cleanup, upload});
						signal?.addEventListener("abort", abort, {once:true});
						if (signal?.aborted) { abort(); return; }
						try {
							chrome.webview.postMessage({type:"call", id, name, args:data.args, binary:!!body});
						} catch (error) {
							pending.delete(id);
							cleanup();
							reject(error);
						}
					});
				} finally { release?.(); }
			},
			on(name, listener) {
				if (typeof name !== "string" || !name) throw new TypeError("event must be a non-empty string");
				if (typeof listener !== "function") throw new TypeError("listener must be a function");
				let set = listeners.get(name);
				if (!set) listeners.set(name, set = new Set());
				set.add(listener);
				return () => {
					set.delete(listener);
					if (!set.size) listeners.delete(name);
				};
			}
		}),
		writable: false,
		configurable: false,
	});
	addEventListener("pagehide", () => {
		closed = true;
		for (const call of pending.values()) { call.cleanup(); call.reject(new Error("document closed")); }
		pending.clear();
		for (const call of uploads.splice(0)) { call.cleanup(); call.reject(new Error("document closed")); }
	});
	addEventListener("dragover", event => {
		if (event.dataTransfer && Array.from(event.dataTransfer.types).includes("Files")) event.preventDefault();
	});
	addEventListener("drop", event => {
		const files = Array.from(event.dataTransfer?.files || []);
		if (files.length && chrome.webview.postMessageWithAdditionalObjects) {
			event.preventDefault();
			chrome.webview.postMessageWithAdditionalObjects({type:"drop"}, files);
		}
	});
})();`

// localURL 判断请求是否属于WebView2宿主来源
// 入参: raw 资源地址
// 返回: bool 是否属于宿主来源
func localURL(raw string) bool {
	return localResource(resourceOrigin, raw)
}

// resourceURL 构造WebView2宿主资源地址
// 入参: path 资源路径
// 返回: string 宿主资源地址
func resourceURL(path string) string {
	return resourceAddress(resourceOrigin, path)
}

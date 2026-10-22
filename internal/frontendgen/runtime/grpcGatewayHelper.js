import * as baseServiceModule from 'BASE_SERVICE_IMPORT_PLACEHOLDER';
// Helper and request import each other; resolve the transport at call time.
const getBaseService = () => baseServiceModule.default || baseServiceModule;

// ==================== UTF-8 Polyfill ====================
function uint8ArrayToString(arr) {
    let str = '';
    for (let i = 0; i < arr.length; i++) {
        const code = arr[i];
        if (code < 0x80) {
            str += String.fromCharCode(code);
        } else if (code < 0xe0) {
            str += String.fromCharCode(((code & 0x1f) << 6) | (arr[++i] & 0x3f));
        } else if (code < 0xf0) {
            str += String.fromCharCode(
                ((code & 0x0f) << 12) | ((arr[++i] & 0x3f) << 6) | (arr[++i] & 0x3f)
            );
        } else {
            const utf32 =
                ((code & 0x07) << 18) |
                ((arr[++i] & 0x3f) << 12) |
                ((arr[++i] & 0x3f) << 6) |
                (arr[++i] & 0x3f);
            const offset = utf32 - 0x10000;
            str += String.fromCharCode(0xd800 + (offset >> 10), 0xdc00 + (offset & 0x3ff));
        }
    }
    return str;
}

let activeAdapter = {};

// ==================== bytes ↔ base64 自动编解码 ====================
// 后端 protojson 把 proto 的 bytes 字段编码为 base64 字符串。生成器按响应/请求消息的
// 描述符算出 bytes 字段路径（reqPaths/respPaths），这里据此在出入站自动转换，
// 业务层从此双向只面对 Uint8Array。多端通用：优先 atob/btoa，无则走手写 base64 表。
const B64_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

function bytesToBase64(bytes) {
    if (typeof btoa === 'function') {
        let binary = '';
        for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
        return btoa(binary);
    }
    let out = '';
    for (let i = 0; i < bytes.length; i += 3) {
        const b0 = bytes[i];
        const b1 = i + 1 < bytes.length ? bytes[i + 1] : 0;
        const b2 = i + 2 < bytes.length ? bytes[i + 2] : 0;
        out += B64_CHARS[b0 >> 2];
        out += B64_CHARS[((b0 & 3) << 4) | (b1 >> 4)];
        out += i + 1 < bytes.length ? B64_CHARS[((b1 & 15) << 2) | (b2 >> 6)] : '=';
        out += i + 2 < bytes.length ? B64_CHARS[b2 & 63] : '=';
    }
    return out;
}

function base64ToBytes(b64) {
    if (typeof atob === 'function') {
        const binary = atob(b64);
        const bytes = new Uint8Array(binary.length);
        for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
        return bytes;
    }
    const clean = b64.replace(/[^A-Za-z0-9+/]/g, '');
    const outLen = Math.floor((clean.length * 3) / 4);
    const bytes = new Uint8Array(outLen);
    let p = 0;
    for (let i = 0; i < clean.length; i += 4) {
        const e0 = B64_CHARS.indexOf(clean[i]);
        const e1 = B64_CHARS.indexOf(clean[i + 1]);
        const e2 = B64_CHARS.indexOf(clean[i + 2]);
        const e3 = B64_CHARS.indexOf(clean[i + 3]);
        const n = (e0 << 18) | (e1 << 12) | ((e2 & 63) << 6) | (e3 & 63);
        if (p < outLen) bytes[p++] = (n >> 16) & 0xff;
        if (e2 !== -1 && p < outLen) bytes[p++] = (n >> 8) & 0xff;
        if (e3 !== -1 && p < outLen) bytes[p++] = n & 0xff;
    }
    return bytes;
}

// 叶子转换：bytes 字段的值可能是单个、数组（repeated）或 map（map<_,bytes>）。
// 解码 base64 串→Uint8Array；编码 Uint8Array→base64 串。先于 object 分支判类型化数组。
function decodeLeaf(v) {
    if (typeof v === 'string') return base64ToBytes(v);
    if (Array.isArray(v)) return v.map(decodeLeaf);
    if (v && typeof v === 'object') {
        for (const k in v) v[k] = decodeLeaf(v[k]);
        return v;
    }
    return v;
}

function encodeLeaf(v) {
    if (v instanceof Uint8Array) return bytesToBase64(v);
    if (v instanceof ArrayBuffer) return bytesToBase64(new Uint8Array(v));
    if (Array.isArray(v)) return v.map(encodeLeaf);
    if (v && typeof v === 'object') {
        for (const k in v) v[k] = encodeLeaf(v[k]);
        return v;
    }
    return v;
}

// 按 key 路径就地改写 obj 上的 bytes 字段。中间层遇数组（repeated message）自动逐元素递归。
function walkBytesPath(node, path, idx, leaf) {
    if (node == null || typeof node !== 'object') return;
    if (Array.isArray(node)) {
        for (const el of node) walkBytesPath(el, path, idx, leaf);
        return;
    }
    const key = path[idx];
    if (idx === path.length - 1) {
        if (key in node) node[key] = leaf(node[key]);
    } else {
        walkBytesPath(node[key], path, idx + 1, leaf);
    }
}

// paths: string[][]（每条是一组 key）。obj 为空或 paths 为空时原样返回。
function applyBytesPaths(obj, paths, leaf) {
    if (!obj || !paths || !paths.length) return obj;
    for (const path of paths) walkBytesPath(obj, path, 0, leaf);
    return obj;
}


export function configureGrpcWeb(adapter) {
    activeAdapter = adapter;
}

// ==================== Standard Browser Stream ====================
function defaultRequestStream(url, data, opts = {}) {
    const baseService = getBaseService();
    const bodyStr = JSON.stringify(data);
    const headers = { 'Content-Type': 'application/json' };

    const buildHeadersFn = baseService.buildHeaders || baseService.getHeaders;
    const finalHeaders = buildHeadersFn ? buildHeadersFn(bodyStr) : headers;

    const baseUrl = baseService.baseUrl || baseService.BASE_URL || "";
    const finalUrl = url.startsWith('http') ? url : (baseUrl ? (baseUrl.endsWith('/') ? baseUrl.slice(0, -1) : baseUrl) + url : url);

    return new Promise((resolve, reject) => {
        fetch(finalUrl, {
            method: 'POST',
            headers: finalHeaders,
            body: bodyStr
        }).then(async (response) => {
            if (response.status !== 200) {
                throw new Error("HTTP error! status: " + response.status);
            }
            const reader = response.body.getReader();
            let buffer = '';

            try {
                while (true) {
                    const { done, value } = await reader.read();
                    if (done) break;

                    buffer += uint8ArrayToString(value);
                    const lines = buffer.split('\n');
                    buffer = lines.pop();

                    for (const line of lines) {
                        if (line.trim() === '') continue;
                        try {
                            const parsed = JSON.parse(line);
                            if (parsed.error) {
                                const err = new Error(parsed.error.message);
                                opts.onError?.(err);
                                reject(err);
                                return;
                            }
                            opts.onChunk?.(parsed.result);
                        } catch (e) {
                            console.error("Failed to parse stream JSON line:", line, e);
                        }
                    }
                }
                opts.onSuccess?.();
                resolve();
            } catch (err) {
                opts.onError?.(err);
                reject(err);
            }
        }).catch((err) => {
            opts.onError?.(err);
            reject(err);
        });
    });
}


// ==================== Export wrapped service ====================
// reqPaths/respPaths 由生成器按 Input/Output 描述符算出（string[][]）；无 bytes 的方法不传。
const service = {
    post: (url, data, opts, reqPaths, respPaths) => {
        if (reqPaths) applyBytesPaths(data, reqPaths, encodeLeaf);
        const p = getBaseService().post(url, data, opts);
        return respPaths ? p.then((r) => applyBytesPaths(r, respPaths, decodeLeaf)) : p;
    },
    get: (url, data, opts) => {
        const baseService = getBaseService();
        if (baseService.get) {
            return baseService.get(url, data, opts);
        }
        return Promise.reject(new Error("GET method not supported by base service"));
    },
    stream: (url, data, opts, reqPaths, respPaths) => {
        if (reqPaths) applyBytesPaths(data, reqPaths, encodeLeaf);
        const reqStream = activeAdapter.requestStream || defaultRequestStream;
        let finalOpts = opts || {};
        if (respPaths && finalOpts.onChunk) {
            const userOnChunk = finalOpts.onChunk;
            finalOpts = { ...finalOpts, onChunk: (chunk) => userOnChunk(applyBytesPaths(chunk, respPaths, decodeLeaf)) };
        }
        return reqStream(url, data, finalOpts);
    },
};

export default service;

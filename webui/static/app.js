/* pdfcpu2 webui 鈥?application logic (no framework, no external deps). */
'use strict';

/* ============================= tiny DOM helpers ============================= */

const $ = (sel) => document.querySelector(sel);

function el(tag, attrs = {}, ...children) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
        if (v === null || v === undefined || v === false) continue;
        if (k === 'class') n.className = v;
        else if (k === 'text') n.textContent = v;
        else if (k === 'html') n.innerHTML = v; // trusted templates only
        else if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2).toLowerCase(), v);
        else if (k === 'value') n.value = v;
        else if (k === 'checked') n.checked = true;
        else if (k === 'disabled') n.disabled = true;
        else if (k === 'selected') n.selected = true;
        else n.setAttribute(k, v);
    }
    for (const c of children.flat()) {
        if (c === null || c === undefined || c === false) continue;
        n.append(c.nodeType ? c : document.createTextNode(c));
    }
    return n;
}

function icon(name, className = '') {
    return `<svg${className ? ` class="${className}"` : ''} aria-hidden="true"><use href="#i-${name}"></use></svg>`;
}

function notify(message, type = 'info') {
    const region = $('#toastRegion');
    if (!region || !message) return;
    const toast = el('div', {class: 'toast ' + type, html: icon(type === 'error' ? 'error' : 'info')});
    toast.append(el('span', {text: String(message)}));
    region.append(toast);
    setTimeout(() => toast.remove(), type === 'error' ? 6500 : 3500);
}

function fmtSize(n) {
    if (!Number.isFinite(n)) return '';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

function fmtBytesLike(v) {
    if (typeof v === 'number') return fmtSize(v);
    return String(v);
}

function t(key) {
    const d = I18N[prefs.lang] || I18N.en;
    const v = d[key];
    // Parameterized entries (e.g. fileCount: (n) => ...) are returned as-is so
    // call sites can invoke them: t('fileCount')(n). Calling them here would
    // return a string and make those call sites throw "not a function".
    if (typeof v === 'function') return v;
    if (v !== undefined) return v;
    const e = I18N.en[key];
    return e !== undefined ? e : key;
}

function applyStaticI18n() {
    document.querySelectorAll('[data-i18n]').forEach((n) => {
        n.textContent = t(n.dataset.i18n);
    });
    document.documentElement.lang = prefs.lang;
}

function debounce(fn, ms) {
    let h;
    return (...args) => {
        clearTimeout(h);
        h = setTimeout(() => fn(...args), ms);
    };
}

/* Surface swallowed errors in the console 鈥?never debug blind. */
function warn(context, err) {
    const msg = err && err.message ? err.message : String(err);
    console.warn('[webui] ' + context + ': ' + msg);
}

/* ============================= preferences ============================= */

const PREFS_KEY = 'pdfcpu2.webui.prefs';
const LAST_VALUES_KEY = 'pdfcpu2.webui.lastValues';

const DEFAULT_PREFS = {lang: 'en', theme: 'light', dpi: 96, autoDl: false};

let prefs = (() => {
    try {
        const p = {...DEFAULT_PREFS, ...JSON.parse(localStorage.getItem(PREFS_KEY) || '{}')};
        if (!I18N[p.lang]) p.lang = 'en';
        return p;
    } catch (_) {
        return {...DEFAULT_PREFS};
    }
})();

let lastValues = (() => {
    try {
        return JSON.parse(localStorage.getItem(LAST_VALUES_KEY) || '{}') || {};
    } catch (_) {
        return {};
    }
})();

function savePrefs() {
    localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
}

function saveLastValues() {
    localStorage.setItem(LAST_VALUES_KEY, JSON.stringify(lastValues));
}

function applyTheme() {
    const systemDark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
    document.documentElement.dataset.theme = prefs.theme === 'system' ? (systemDark ? 'dark' : 'light') : prefs.theme;
    const button = $('#themeBtn');
    if (button) {
        button.innerHTML = icon(document.documentElement.dataset.theme === 'dark' ? 'sun' : 'moon');
        button.title = t('toggleTheme');
        button.setAttribute('aria-label', t('toggleTheme'));
    }
}

/* ============================= engine (worker RPC) ============================= */

const engine = {
    worker: null,
    ready: false,
    failed: false,
    version: '',
    pending: new Map(),
    seq: 0,

    init() {
        this.worker = new Worker('./worker.js');
        this.worker.onmessage = (ev) => {
            const m = ev.data || {};
            if (m.type === 'ready') {
                this.ready = true;
                this.version = m.version || '';
                renderEngineStatus();
                renderFiles();
                updateRunState();
                return;
            }
            if (m.type === 'error') {
                this.failed = true;
                this.errorText = m.error;
                console.error('[webui] engine:', m.error);
                renderEngineStatus();
                return;
            }
            if (m.type === 'rpc' && this.pending.has(m.id)) {
                const {resolve, reject} = this.pending.get(m.id);
                this.pending.delete(m.id);
                if (m.ok) resolve(m.res);
                else reject(new Error(m.error || 'engine error'));
            }
        };
        this.worker.onerror = (e) => {
            this.failed = true;
            this.errorText = e.message || 'worker failed';
            console.error('[webui] worker error:', this.errorText);
            renderEngineStatus();
        };
    },

    call(fn, ...args) {
        if (!this.ready) return Promise.reject(new Error(t('engineLoading')));
        return new Promise((resolve, reject) => {
            const id = 'r' + ++this.seq;
            this.pending.set(id, {resolve, reject});
            this.worker.postMessage({id, fn, args});
        });
    },
};

function renderEngineStatus() {
    const box = $('#engineStatus');
    if (!box) return;
    const dot = $('#engineDot');
    box.className = 'engine-status';
    if (dot) dot.className = 'engine-dot';
    if (engine.failed) {
        box.classList.add('err');
        if (dot) dot.classList.add('err');
        box.textContent = t('engineFailed') + (engine.errorText ? ' 鈥?' + engine.errorText : '');
    } else if (engine.ready) {
        box.classList.add('ok');
        if (dot) dot.classList.add('ok');
        box.textContent = t('engineReady');
        box.title = engine.version || '';
    } else {
        box.classList.add('load');
        box.textContent = t('engineLoading');
    }
    updateRunState();
}

/* ============================= files ============================= */

const state = {
    files: [], // {id,name,kind,size,data,pageCount,encrypted,probeErr,isResult,ts}
    seq: 1,
    activeId: null,
    multiIds: [],
    tool: 'encrypt',
    batchOp: 'encrypt',
    attachmentDrafts: [],
    busy: false,
};

const IMAGE_EXT = /\.(png|jpe?g|jpe|tiff?|webp|gif)$/i;

function kindOf(name) {
    if (/\.pdf$/i.test(name)) return 'pdf';
    if (IMAGE_EXT.test(name)) return 'image';
    return 'other';
}

function byId(id) {
    return state.files.find((f) => f.id === id) || null;
}

function thumbCacheKey(e, page) {
    return e.id + ':' + page + ':' + prefs.dpi + ':' + e.data.length;
}

const thumbCache = new Map();

async function thumbURL(e, page = 1) {
    if (!e) return null;
    if (e.kind === 'image') {
        const key = thumbCacheKey(e, 0);
        if (thumbCache.has(key)) return thumbCache.get(key);
        const url = URL.createObjectURL(new Blob([e.data], {type: e.mime || 'image/*'}));
        thumbCache.set(key, url);
        return url;
    }
    if (e.kind !== 'pdf' || e.encrypted || !engine.ready) return null;
    const key = thumbCacheKey(e, page);
    if (thumbCache.has(key)) return thumbCache.get(key);
    const t0 = Date.now();
    let r;
    try {
        r = await engine.call('renderPage', e.data, page, prefs.dpi);
    } catch (err) {
        warn('renderPage ' + e.name + ' p' + page, err);
        return null;
    }
    if (!r || !r.ok) {
        warn('renderPage ' + e.name + ' p' + page, new Error((r && r.error) || 'no result'));
        return null;
    }
    console.debug('[webui] renderPage %s p%d: %d ms (%d bytes)', e.name, page, Date.now() - t0, r.data.length);
    const url = URL.createObjectURL(new Blob([r.data], {type: 'image/png'}));
    thumbCache.set(key, url);
    return url;
}

async function addFiles(fileList) {
    const added = [];
    const values = currentFormValues(currentTool());
    const pick = effectivePick(currentTool(), values);
    for (const f of fileList) {
        let data;
        try {
            data = new Uint8Array(await f.arrayBuffer());
        } catch (err) {
            console.error('[webui] read ' + f.name + ':', err);
            notify(f.name + ': ' + err.message, 'error');
            continue;
        }
        const e = {
            id: 'f' + state.seq++,
            name: f.name || 'file',
            kind: kindOf(f.name || ''),
            mime: f.type || '',
            size: data.length,
            data,
            pageCount: 0,
            encrypted: false,
            probeErr: '',
            isResult: false,
            ts: Date.now(),
        };
        state.files.push(e);
        added.push(e);
        if (e.kind === 'pdf') probeFile(e);
        if (!isSelectable(e, currentTool(), values)) continue;
        if (pick === 'multi') {
            if (!state.multiIds.includes(e.id)) state.multiIds.push(e.id);
        } else if (!state.activeId || added.length === 1) {
            state.activeId = e.id;
        }
    }
    renderTool();
    renderToolForm(); // refresh image pickers after an image is added
    updateRunState();
    return added;
}

/* Re-render the dynamic tool UI. Deliberately does NOT rebuild the form so
   in-progress typing survives selection changes. */
function renderTool() {
    renderToolChrome();
    renderFiles();
    renderToolTarget();
    updateRunState();
}

async function probeFile(e) {
    try {
        const r = await engine.call('probe', e.data);
        if (r.ok) {
            e.pageCount = r.pageCount || 0;
            e.encrypted = !!r.encrypted;
        } else {
            e.probeErr = r.error;
            warn('probe ' + e.name, new Error(r.error));
        }
    } catch (err) {
        e.probeErr = err.message;
        warn('probe ' + e.name, err);
    }
    renderFiles();
    renderTool();
}

function addResultEntry(name, data, kind) {
    const e = {
        id: 'f' + state.seq++,
        name,
        kind: kind || kindOf(name),
        size: data.length,
        data,
        pageCount: 0,
        encrypted: false,
        probeErr: '',
        isResult: true,
        ts: Date.now(),
    };
    state.files.push(e);
    if (e.kind === 'pdf') probeFile(e);
    return e;
}

function removeFile(id) {
    state.files = state.files.filter((f) => f.id !== id);
    if (state.activeId === id) state.activeId = null;
    state.multiIds = state.multiIds.filter((x) => x !== id);
    renderFiles();
    renderTool();
    updateRunState();
}

/* ============================= selection model ============================= */

function selectSingle(id) {
    state.activeId = id;
    renderFiles();
    renderTool();
}

function toggleMulti(id) {
    const i = state.multiIds.indexOf(id);
    if (i >= 0) state.multiIds.splice(i, 1);
    else state.multiIds.push(id);
    renderFiles();
    renderTool();
    updateRunState();
}

function moveMulti(id, dir) {
    const i = state.multiIds.indexOf(id);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= state.multiIds.length) return;
    const tmp = state.multiIds[i];
    state.multiIds[i] = state.multiIds[j];
    state.multiIds[j] = tmp;
    renderFiles();
    renderTool();
}

/* ============================= downloads ============================= */

function download(name, data, mime) {
    const url = URL.createObjectURL(new Blob([data], {type: mime || 'application/octet-stream'}));
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
}

/* ============================= tool registry ============================= */

const GROUPS = [
    ['security', ['encrypt', 'decrypt']],
    ['content', ['replace', 'ocr', 'solidify', 'obfuscate', 'optimize', 'totext', 'watermark']],
    ['pages', ['split', 'merge', 'img2pdf']],
    ['advanced', ['attachments', 'signatures']],
    ['batch', ['batch']],
];

const BATCH_OPS = ['replace', 'encrypt', 'decrypt', 'ocr', 'solidify', 'obfuscate', 'optimize', 'totext', 'watermark'];

const WM_POSITIONS = [
    ['c', 'c'], ['tl', 'tl'], ['tc', 'tc'], ['tr', 'tr'],
    ['l', 'l'], ['r', 'r'], ['bl', 'bl'], ['bc', 'bc'], ['br', 'br'],
];

const TOOLS = {
    encrypt: {
        title: 'tool_encrypt', desc: 'tool_encrypt_desc', icon: 'encrypt',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [
            {k: 'ownerPw', t: 'password', label: 'f_encrypt_ownerPw', req: true, hint: 'f_encrypt_ownerPw_hint'},
            {k: 'userPw', t: 'password', label: 'f_encrypt_userPw', hint: 'f_encrypt_userPw_hint'},
            {
                k: 'aes',
                t: 'select',
                label: 'f_encrypt_aes',
                def: 'true',
                opts: [['true', 'f_encrypt_aes_aes'], ['false', 'f_encrypt_aes_rc4']]
            },
            {
                k: 'keyLength',
                t: 'select',
                label: 'f_encrypt_keyLength',
                def: '256',
                opts: [['256', '256 bit'], ['128', '128 bit'], ['40', '40 bit']]
            },
            {
                k: 'perm',
                t: 'select',
                label: 'f_encrypt_perm',
                def: 'print',
                opts: [['none', 'f_encrypt_perm_none'], ['print', 'f_encrypt_perm_print'], ['all', 'f_encrypt_perm_all']]
            },
        ],
        exec: (v, file) => engine.call('encrypt', file.data, v),
    },

    decrypt: {
        title: 'tool_decrypt', desc: 'tool_decrypt_desc', icon: 'decrypt',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [
            {k: 'password', t: 'password', label: 'f_decrypt_password', hint: 'f_decrypt_password_hint'},
        ],
        exec: (v, file) => engine.call('decrypt', file.data, v),
    },

    replace: {
        title: 'tool_replace', desc: 'tool_replace_desc', icon: 'replace',
        pick: 'single', filter: 'pdf', batch: true, note: 'f_replace_note',
        fields: [
            {k: 'find', t: 'text', label: 'f_replace_find', req: true, hint: 'f_replace_find_hint'},
            {k: 'replace', t: 'text', label: 'f_replace_with', hint: 'f_replace_with_hint'},
            {k: 'pages', t: 'text', label: 'fld_pages', hint: 'fld_pages_hint', placeholder: '1-3, 5'},
        ],
        exec: (v, file) => engine.call('replace', file.data, v),
    },

    ocr: {
        title: 'tool_ocr', desc: 'tool_ocr_desc', icon: 'ocr',
        pick: 'single', filter: 'pdf', batch: true, note: 'f_ocr_note',
        fields: [
            {k: 'pages', t: 'text', label: 'fld_pages', hint: 'fld_pages_hint', placeholder: '1-3, 5'},
        ],
        exec: (v, file) => engine.call('ocr', file.data, v),
    },

    solidify: {
        title: 'tool_solidify', desc: 'tool_solidify_desc', icon: 'solidify',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [
            {k: 'pages', t: 'text', label: 'fld_pages', hint: 'fld_pages_hint'},
            {k: 'dpi', t: 'number', label: 'fld_dpi', def: 150, min: 36, max: 600, step: 10},
            {
                k: 'format',
                t: 'select',
                label: 'f_solidify_format',
                def: 'png',
                opts: [['png', 'f_solidify_format_png'], ['jpg', 'f_solidify_format_jpg']]
            },
            {
                k: 'quality',
                t: 'number',
                label: 'fld_quality',
                def: 85,
                min: 1,
                max: 100,
                when: (v) => v.format === 'jpg'
            },
        ],
        exec: (v, file) => engine.call('solidify', file.data, v),
    },

    obfuscate: {
        title: 'tool_obfuscate', desc: 'tool_obfuscate_desc', icon: 'obfuscate',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [],
        exec: (_v, file) => engine.call('obfuscate', file.data),
    },

    optimize: {
        title: 'tool_optimize', desc: 'tool_optimize_desc', icon: 'optimize',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [],
        exec: (_v, file) => engine.call('optimize', file.data),
    },

    totext: {
        title: 'tool_totext', desc: 'tool_totext_desc', icon: 'totext',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [
            {k: 'pages', t: 'text', label: 'fld_pages', hint: 'fld_pages_hint'},
            {k: 'sortByPosition', t: 'checkbox', label: 'f_totext_sortByPosition'},
            {k: 'normalizeWhitespace', t: 'checkbox', label: 'f_totext_normalizeWhitespace'},
        ],
        exec: (v, file) => engine.call('totext', file.data, v),
    },

    watermark: {
        title: 'tool_watermark', desc: 'tool_watermark_desc', icon: 'watermark',
        pick: 'single', filter: 'pdf', batch: true,
        fields: [
            {
                k: 'mode',
                t: 'select',
                label: 'f_wm_mode',
                def: 'text',
                opts: [['text', 'f_wm_mode_text'], ['image', 'f_wm_mode_image']]
            },
            {
                k: 'text',
                t: 'text',
                label: 'f_wm_text',
                req: true,
                when: (v) => v.mode !== 'image',
                placeholder: 'DRAFT'
            },
            {
                k: 'imageId',
                t: 'imagefile',
                label: 'f_wm_imageId',
                hint: 'f_wm_imageId_hint',
                when: (v) => v.mode === 'image'
            },
            {
                k: 'onTop',
                t: 'select',
                label: 'f_wm_onTop',
                def: 'true',
                opts: [['true', 'f_wm_onTop_stamp'], ['false', 'f_wm_onTop_watermark']]
            },
            {k: 'position', t: 'select', label: 'f_wm_position', def: 'c', opts: WM_POSITIONS},
            {
                k: 'rotation',
                t: 'select',
                label: 'f_wm_rotation',
                def: '',
                opts: [['', '—'], ['45', '45°'], ['90', '90°'], ['-45', '-45°'], ['diagonal1', 'f_wm_d1'], ['diagonal2', 'f_wm_d2']]
            },
            {k: 'opacity', t: 'number', label: 'f_wm_opacity', def: 1, min: 0, max: 1, step: 0.05},
            {
                k: 'scale',
                t: 'number',
                label: 'f_wm_scale',
                def: 0,
                min: 0,
                max: 10,
                step: 0.05,
                hint: 'f_wm_scale_hint'
            },
            {k: 'scaleAbs', t: 'checkbox', label: 'f_wm_scaleAbs', when: (v) => v.mode === 'image'},
            {k: 'offsetX', t: 'number', label: 'f_wm_offsetX', def: 0},
            {k: 'offsetY', t: 'number', label: 'f_wm_offsetY', def: 0},
            {k: 'fontName', t: 'text', label: 'f_wm_fontName', def: 'Helvetica', when: (v) => v.mode !== 'image'},
            {
                k: 'fontSize',
                t: 'number',
                label: 'f_wm_fontSize',
                def: 0,
                min: 0,
                hint: 'f_wm_fontSize_hint',
                when: (v) => v.mode !== 'image'
            },
            {k: 'color', t: 'color', label: 'f_wm_color', def: '#808080', when: (v) => v.mode !== 'image'},
        ],
        exec: (v, file) => {
            if (v.mode === 'image') {
                const logo = byId(v.imageId);
                if (!logo) return Promise.reject(new Error(t('f_wm_imageId')));
                return engine.call('watermarkImage', file.data, logo.data, logo.name, v);
            }
            return engine.call('watermarkText', file.data, v);
        },
    },

    split: {
        title: 'tool_split', desc: 'tool_split_desc', icon: 'split',
        pick: 'single', filter: 'pdf',
        fields: [
            {
                k: 'mode',
                t: 'select',
                label: 'f_split_mode',
                def: 'span',
                opts: [['span', 'f_split_mode_span'], ['pages', 'f_split_mode_pages'], ['bookmarks', 'f_split_mode_bookmarks']]
            },
            {k: 'span', t: 'number', label: 'f_split_span', def: 1, min: 1, when: (v) => v.mode === 'span'},
            {
                k: 'pageNrs',
                t: 'text',
                label: 'f_split_pageNrs',
                hint: 'f_split_pageNrs_hint',
                placeholder: '3, 7, 21',
                when: (v) => v.mode === 'pages'
            },
        ],
        exec: (v, file) => engine.call('split', file.data, file.name, v),
    },

    merge: {
        title: 'tool_merge', desc: 'tool_merge_desc', icon: 'merge',
        pick: 'multi', filter: 'pdf', seams: true,
        fields: [
            {k: 'divider', t: 'checkbox', label: 'f_merge_divider'},
            {k: 'optimize', t: 'checkbox', label: 'f_merge_optimize'},
        ],
        exec: (v, _file, files) => engine.call('merge', files.map((f) => f.data), v),
    },

    img2pdf: {
        title: 'tool_img2pdf', desc: 'tool_img2pdf_desc', icon: 'img2pdf',
        pick: 'multi', filter: 'image',
        fields: [
            {
                k: 'paper',
                t: 'select',
                label: 'f_i2p_paper',
                def: 'A4',
                opts: ['A3', 'A4', 'A5', 'A6', 'Letter', 'Legal', 'Ledger', 'Tabloid']
            },
            {
                k: 'fit',
                t: 'select',
                label: 'f_i2p_fit',
                def: 'stretch',
                opts: [['stretch', 'f_i2p_fit_stretch'], ['contain', 'f_i2p_fit_contain']]
            },
            {
                k: 'scale',
                t: 'number',
                label: 'f_i2p_scale',
                def: 0.9,
                min: 0.05,
                max: 1,
                step: 0.05,
                when: (v) => v.fit === 'contain'
            },
            {k: 'gray', t: 'checkbox', label: 'f_i2p_gray'},
            {k: 'sepia', t: 'checkbox', label: 'f_i2p_sepia'},
        ],
        exec: (v, _file, files) => engine.call('img2pdf', files.map((f) => ({name: f.name, data: f.data})), v),
    },

    attachments: {
        title: 'tool_attachments', desc: 'tool_attachments_desc', icon: 'attachments',
        // Adding attachments uses a dedicated target PDF + unrestricted file picker.
        // It never overloads the workspace's PDF/image selection model.
        pick: 'single', filter: 'pdf',
        fields: [
            {
                k: 'action',
                t: 'select',
                label: 'f_att_action',
                def: 'list',
                opts: [['list', 'f_att_action_list'], ['add', 'f_att_action_add'], ['remove', 'f_att_action_remove'], ['extract', 'f_att_action_extract']]
            },
            {k: 'desc', t: 'text', label: 'f_att_desc', when: (v) => v.action === 'add'},
            {
                k: 'names',
                t: 'text',
                label: 'f_att_names',
                hint: 'f_att_names_hint',
                when: (v) => v.action === 'remove' || v.action === 'extract'
            },
        ],
        exec: (v, file, files) => {
            if (v.action === 'list') return engine.call('attList', file.data);
            if (v.action === 'add') {
                if (files.length < 2) return Promise.reject(new Error(t('att_add_need')));
                const target = files[0];
                const rest = files.slice(1);
                return engine.call('attAdd', target.data, rest.map((f) => ({
                    name: f.name,
                    data: f.data
                })), v.desc || '');
            }
            if (v.action === 'remove') return engine.call('attRemove', file.data, v.names || '');
            return engine.call('attExtract', file.data, v.names || '');
        },
    },

    signatures: {
        title: 'tool_signatures', desc: 'tool_signatures_desc', icon: 'signatures',
        pick: 'single', filter: 'pdf', note: 'sig_note',
        fields: [
            {
                k: 'action',
                t: 'select',
                label: 'f_sig_action',
                def: 'validate',
                opts: [['validate', 'f_sig_action_validate'], ['remove', 'f_sig_action_remove']]
            },
            {k: 'all', t: 'checkbox', label: 'f_sig_all', when: (v) => v.action === 'validate'},
        ],
        exec: (v, file) => (v.action === 'remove'
            ? engine.call('sigRemove', file.data)
            : engine.call('sigValidate', file.data, !!v.all)),
    },

    batch: {
        title: 'tool_batch', desc: 'tool_batch_desc', icon: 'batch',
        pick: 'multi', filter: 'pdf',
        fields: () => {
            const opField = {
                k: 'op',
                t: 'select',
                label: 'f_batch_op',
                def: state.batchOp,
                opts: BATCH_OPS.map((o) => [o, 'tool_' + o])
            };
            const sub = (TOOLS[state.batchOp].fields || []).map((f) => ({...f}));
            return [opField, ...sub];
        },
        exec: (v, _file, files, logo, logoName) => engine.call('batch', v.op, files.map((f) => ({
            name: f.name,
            data: f.data
        })), v, logo, logoName),
    },
};

function currentTool() {
    return TOOLS[state.tool];
}

function isSelectable(e, tool, values) {
    if (!tool) return true;
    let filter = tool.filter;
    if (typeof filter === 'function') filter = filter(values || {});
    if (!filter) return true;
    return e.kind === filter;
}

function effectivePick(tool, values) {
    const p = tool.pick;
    if (typeof p === 'function') return p(values || {});
    return p || 'single';
}

/* ============================= form rendering ============================= */

function fieldKey(toolId, k) {
    return toolId + '.' + k;
}

function lastValue(toolId, field) {
    const saved = lastValues[fieldKey(toolId, field.k)];
    if (saved !== undefined && saved !== null && saved !== '') return saved;
    return field.def !== undefined ? field.def : '';
}

function renderField(toolId, f) {
    const id = 'fld_' + toolId + '_' + f.k;
    const labelText = t(f.label || fieldKey(toolId, f.k));
    const label = el('label', {for: id, class: f.req ? 'req' : ''}, labelText);
    let input;

    switch (f.t) {
        case 'password':
            input = el('input', {id, type: 'password', value: lastValue(toolId, f)});
            break;
        case 'number':
            input = el('input', {id, type: 'number', value: lastValue(toolId, f)});
            if (f.min !== undefined) input.min = f.min;
            if (f.max !== undefined) input.max = f.max;
            if (f.step !== undefined) input.step = f.step;
            break;
        case 'checkbox': {
            const cb = el('input', {id, type: 'checkbox'});
            cb.checked = lastValue(toolId, f) === true || lastValue(toolId, f) === 'true';
            cb.dataset.k = f.k;
            return el('div', {class: 'field inline'}, el('label', {for: id}, labelText), cb);
        }
        case 'select': {
            input = el('select', {id});
            for (const opt of f.opts) {
                const val = opt[0], lab = opt[1];
                const o = el('option', {value: val});
                o.textContent = I18N[prefs.lang] && I18N[prefs.lang][lab] || I18N.en[lab] || lab;
                if (String(lastValue(toolId, f)) === String(val)) o.selected = true;
                input.append(o);
            }
            break;
        }
        case 'imagefile': {
            input = el('select', {id});
            input.append(el('option', {value: ''}, t('chooseImage')));
            for (const e of state.files.filter((x) => x.kind === 'image')) {
                const o = el('option', {value: e.id}, e.name);
                if (String(lastValue(toolId, f)) === e.id) o.selected = true;
                input.append(o);
            }
            const browse = el('button', {
                class: 'icon-button',
                type: 'button',
                title: t('chooseImage'),
                html: icon('plus')
            });
            browse.addEventListener('click', () => {
                const picker = el('input', {type: 'file', accept: 'image/*', hidden: true});
                picker.addEventListener('change', async () => {
                    if (!picker.files || !picker.files[0]) return;
                    const added = await addFiles([picker.files[0]]);
                    const image = added.find((x) => x.kind === 'image');
                    if (image) {
                        lastValues[fieldKey(toolId, f.k)] = image.id;
                        saveLastValues();
                        renderToolForm();
                    }
                });
                document.body.append(picker);
                picker.click();
                setTimeout(() => picker.remove(), 60000);
            });
            input.dataset.k = f.k;
            const control = el('div', {class: 'image-file-control'}, input, browse);
            const imageWrap = el('div', {class: 'field wide'}, label, control);
            if (f.hint) imageWrap.append(el('span', {class: 'hint'}, t(f.hint)));
            return imageWrap;
        }
        case 'color':
            input = el('input', {id, type: 'color', value: lastValue(toolId, f) || '#808080'});
            break;
        default:
            input = el('input', {id, type: 'text', value: lastValue(toolId, f)});
    }
    if (f.placeholder) input.placeholder = f.placeholder;
    input.dataset.k = f.k;
    if (f.req) input.required = true;
    const wrap = el('div', {class: 'field' + (f.wide ? ' wide' : '')}, label, input);
    if (f.hint) wrap.append(el('span', {class: 'hint'}, t(f.hint)));
    return wrap;
}

function currentFormValues(tool, form) {
    tool = tool || currentTool();
    form = form || $('#toolForm');
    const values = {};
    const fields = typeof tool.fields === 'function' ? tool.fields() : tool.fields;
    for (const f of fields) {
        const input = form.querySelector('[data-k="' + f.k + '"]');
        if (!input) continue;
        if (f.t === 'checkbox') values[f.k] = input.checked;
        else values[f.k] = input.value;
    }
    return values;
}

function updateConditionalFields(tool, fields, form) {
    const values = currentFormValues(tool, form);
    for (const f of fields) {
        if (!f.when) continue;
        const node = form.querySelector('[data-when="' + f.k + '"]');
        if (node) node.hidden = !f.when(values);
    }
}

function renderToolForm() {
    const tool = currentTool();
    const toolId = state.tool;
    const form = $('#toolForm');
    form.textContent = '';
    const fields = typeof tool.fields === 'function' ? tool.fields() : tool.fields;
    const advancedStart = toolId === 'watermark' ? 5 : (toolId === 'encrypt' ? 2 : -1);
    let advanced = null;

    fields.forEach((f, index) => {
        const w = renderField(toolId, f);
        if (f.when) w.dataset.when = f.k;
        const input = w.querySelector('input,select,textarea');
        if (input) input.addEventListener('change', () => {
            if (f.k === 'op' && toolId === 'batch') {
                state.batchOp = currentFormValues(tool, form).op || 'encrypt';
                renderToolForm();
                renderToolChrome();
                return;
            }
            updateConditionalFields(tool, fields, form);
            renderFiles();
            renderToolTarget();
            updateRunState();
        });
        if (index === advancedStart) {
            advanced = el('details', {class: 'advanced-options'});
            advanced.append(el('summary', {}, t('advancedOptions')), el('div', {class: 'advanced-grid'}));
            form.append(advanced);
        }
        (advanced ? advanced.children[1] : form).append(w);
    });

    updateConditionalFields(tool, fields, form);
    if (tool.note) form.append(el('p', {class: 'note wide', html: icon('info')}, el('span', {text: t(tool.note)})));
    updateRunState();
}

/* ============================= tool target bar ============================= */

function targetFileRow(file, changeAction) {
    const row = el('div', {class: 'target-file'});
    row.append(el('span', {class: 'target-file-icon', html: icon(file.kind === 'image' ? 'image' : 'file')}));
    const details = [fmtSize(file.size)];
    if (file.pageCount) details.push(file.pageCount + ' ' + t('pages'));
    if (file.encrypted) details.push(t('encrypted'));
    row.append(el('div', {class: 'target-file-info'},
        el('b', {text: file.name, title: file.name}), el('span', {text: details.join(' · ')})));
    if (changeAction) row.append(el('button', {
        class: 'target-change',
        type: 'button',
        onClick: changeAction
    }, t('change')));
    return row;
}

async function addAttachmentFiles(fileList) {
    for (const f of fileList) {
        try {
            const data = new Uint8Array(await f.arrayBuffer());
            state.attachmentDrafts.push({name: f.name || 'attachment', data, size: data.length});
        } catch (err) {
            notify(f.name + ': ' + err.message, 'error');
        }
    }
    renderToolTarget();
    updateRunState();
}

function renderAttachmentTarget(box, active) {
    if (active) box.append(targetFileRow(active));
    else box.append(el('div', {class: 'target-empty', html: icon('file')}, el('span', {text: t('att_choose_target')})));
    const intake = el('div', {class: 'attachment-intake'});
    intake.append(el('div', {class: 'attachment-intake-head'},
        el('div', {}, el('strong', {text: t('att_files_to_add')}), el('p', {text: t('att_any_type')})),
        el('button', {
            class: 'secondary-button',
            type: 'button',
            onClick: () => $('#attachmentInput').click()
        }, t('chooseFiles'))));
    if (state.attachmentDrafts.length) {
        const list = el('div', {class: 'attachment-list'});
        state.attachmentDrafts.forEach((f, index) => {
            list.append(el('span', {class: 'attachment-chip'}, el('span', {text: f.name + ' · ' + fmtSize(f.size)}),
                el('button', {
                    type: 'button', title: t('remove'), html: icon('close'), onClick: () => {
                        state.attachmentDrafts.splice(index, 1);
                        renderToolTarget();
                        updateRunState();
                    }
                })));
        });
        intake.append(list);
    }
    ['dragenter', 'dragover'].forEach((name) => intake.addEventListener(name, (ev) => {
        ev.preventDefault();
        intake.classList.add('drag');
    }));
    ['dragleave', 'drop'].forEach((name) => intake.addEventListener(name, (ev) => {
        ev.preventDefault();
        intake.classList.remove('drag');
    }));
    intake.addEventListener('drop', (ev) => {
        if (ev.dataTransfer && ev.dataTransfer.files.length) addAttachmentFiles([...ev.dataTransfer.files]);
    });
    box.append(intake);
}

function renderToolTarget() {
    const tool = currentTool();
    const box = $('#toolTarget');
    if (!box) return;
    box.textContent = '';
    const values = currentFormValues(tool);
    const pick = effectivePick(tool, values);
    const files = state.multiIds.map(byId).filter(Boolean);
    const active = byId(state.activeId);

    if (state.tool === 'attachments' && values.action === 'add') {
        renderAttachmentTarget(box, active);
        return;
    }
    if (pick === 'single') {
        box.append(active ? targetFileRow(active) : el('div', {class: 'target-empty', html: icon('upload')},
            el('span', {text: t('pickOne')})));
        return;
    }

    const head = el('div', {class: 'order-head'}, el('span', {text: t('orderHint')}));
    if (files.length) head.append(el('button', {
        class: 'text-button', type: 'button', onClick: () => {
            state.multiIds = [];
            renderFiles();
            renderToolTarget();
            updateRunState();
        }
    }, t('clearSelection')));
    box.append(head);
    if (!files.length) {
        box.append(el('div', {class: 'target-empty', html: icon('plus')}, el('span', {text: t('pickMany')})));
        return;
    }
    const list = el('div', {class: 'order-list'});
    files.forEach((f, i) => {
        const item = el('div', {class: 'order-item', draggable: 'true', 'data-id': f.id});
        item.append(el('span', {class: 'drag-handle', html: icon('grip'), title: t('dragToOrder')}),
            el('span', {class: 'order-number'}, String(i + 1)),
            el('span', {class: 'order-name', title: f.name}, f.name));
        const actions = el('span', {class: 'order-actions'});
        actions.append(
            el('button', {
                type: 'button',
                title: t('moveUp'),
                disabled: i === 0,
                html: icon('up'),
                onClick: () => moveMulti(f.id, -1)
            }),
            el('button', {
                type: 'button',
                title: t('moveDown'),
                disabled: i === files.length - 1,
                html: icon('down'),
                onClick: () => moveMulti(f.id, 1)
            }),
            el('button', {type: 'button', title: t('remove'), html: icon('close'), onClick: () => toggleMulti(f.id)}));
        item.append(actions);
        item.addEventListener('dragstart', (ev) => {
            item.classList.add('dragging');
            ev.dataTransfer.setData('text/plain', f.id);
            ev.dataTransfer.effectAllowed = 'move';
        });
        item.addEventListener('dragend', () => item.classList.remove('dragging'));
        item.addEventListener('dragover', (ev) => {
            ev.preventDefault();
            item.classList.add('drag-over');
        });
        item.addEventListener('dragleave', () => item.classList.remove('drag-over'));
        item.addEventListener('drop', (ev) => {
            ev.preventDefault();
            item.classList.remove('drag-over');
            const fromId = ev.dataTransfer.getData('text/plain');
            if (!fromId || fromId === f.id) return;
            const ordered = state.multiIds.filter((id) => id !== fromId);
            ordered.splice(ordered.indexOf(f.id), 0, fromId);
            state.multiIds = ordered;
            renderFiles();
            renderToolTarget();
        });
        list.append(item);
    });
    box.append(list);
    if (tool.seams && files.length >= 2) box.append(el('button', {
        class: 'text-button seams-button', type: 'button', onClick: renderSeams
    }, t('previewSeams')));
}

async function renderSeams() {
    const files = state.multiIds.map(byId).filter(Boolean);
    const old = $('#seamStrip');
    if (old) old.remove();
    const strip = el('div', {class: 'thumb-strip wide', id: 'seamStrip'});
    for (let i = 0; i + 1 < files.length; i++) {
        const a = files[i], b = files[i + 1];
        const seam = el('div', {class: 'seam'});
        for (const [f, page, cap] of [[a, a.pageCount || 1, `« ${f1(a)} p.${a.pageCount || 1}`], [b, 1, `» ${f1(b)} p.1`]]) {
            const holder = el('div', {class: 'seam-page'});
            holder.append(el('div', {class: 'file-placeholder', html: icon('file')}));
            seam.append(holder, el('div', {class: 'cap'}, cap));
            thumbURL(f, page).then((u) => {
                if (u) {
                    holder.textContent = '';
                    holder.append(el('img', {src: u, alt: cap}));
                }
            }).catch((err) => warn('seam thumb ' + f.name + ' p' + page, err));
        }
        strip.append(seam);
    }
    $('#toolTarget').append(strip);
}

function f1(f) {
    return f.name.length > 18 ? f.name.slice(0, 16) + '…' : f.name;
}

/* ============================= run + result ============================= */

function updateRunState() {
    const button = $('#runBtn');
    if (!button || !currentTool()) return;
    const tool = currentTool();
    const values = currentFormValues(tool);
    const pick = effectivePick(tool, values);
    const count = pick === 'multi' ? state.multiIds.length : (byId(state.activeId) ? 1 : 0);
    let hint = count ? (pick === 'multi' ? t('filesReady')(count) : t('fileReady')) : t('selectToContinue');
    let enough = count > 0;
    if (state.tool === 'merge' && count < 2) {
        enough = false;
        hint = t('mergeNeedTwo');
    }
    if (state.tool === 'attachments' && values.action === 'add' && !state.attachmentDrafts.length) {
        enough = false;
        hint = count ? t('att_add_files_continue') : t('selectToContinue');
    }
    if (!engine.ready) hint = t('engineLoadingShort');
    button.disabled = state.busy || !engine.ready || !enough;
    const hintNode = $('#runHint');
    if (hintNode) hintNode.textContent = hint;
    const label = $('#runLabel');
    if (label) label.textContent = t('run_' + state.tool);
}

function setBusy(on, textKey) {
    state.busy = on;
    $('#busy').hidden = !on;
    $('#busyText').textContent = t(textKey || 'running');
    updateRunState();
}

function resultFileName(res, sourceName) {
    let name = res.name || 'out.bin';
    if (name.startsWith('_') || name === '.txt') {
        const base = (sourceName || 'document').replace(/\.[^.]+$/, '');
        name = base + name;
    }
    return name;
}

const DETAIL_LABELS = {
    inSize: 'd_inSize', outSize: 'd_outSize', savedPct: 'd_savedPct',
    imagesProcessed: 'd_imagesProcessed', fontFiles: 'd_fontFiles', toUnicode: 'd_toUnicode',
    skipped: 'd_skipped', files: 'd_files', chars: 'd_chars', pages: 'd_pages',
    added: 'd_added', removed: 'd_removed', algorithm: 'd_algorithm', keyLength: 'd_keyLength',
    perm: 'd_perm', dpi: 'd_dpi', format: 'd_format', count: 'd_count', inputs: 'd_inputs',
    occurrences: 'd_occurrences',
};

function kvTable(details, skip) {
    const rows = [];
    for (const [k, v] of Object.entries(details || {})) {
        if (skip && skip.includes(k)) continue;
        if (k === 'text' || k === 'report' || k === 'name' || k === 'data') continue;
        if (typeof v === 'object' && v !== null && !Array.isArray(v)) continue;
        let val;
        if (k === 'inSize' || k === 'outSize') val = fmtSize(v);
        else if (k === 'savedPct') val = v + ' %';
        else if (Array.isArray(v)) val = v.join(', ');
        else val = String(v);
        if (val === '') continue;
        rows.push([t(DETAIL_LABELS[k] || k), val]);
    }
    if (!rows.length) return null;
    const dl = el('dl', {class: 'kv'});
    for (const [a, b] of rows) dl.append(el('dt', {text: a}), el('dd', {text: b}));
    return dl;
}

function showResult(node) {
    const box = $('#result');
    box.textContent = '';
    box.hidden = false;
    box.append(node);
}

function resultHead(ok, title, sub) {
    const head = el('div', {class: 'result-head' + (ok ? '' : ' err')});
    head.append(el('span', {class: 'result-status-icon', html: icon(ok ? 'check' : 'error')}));
    const copy = el('div', {}, el('div', {class: 'result-title'}, ok ? t('done') : t('failed')));
    if (title) copy.append(el('span', {class: 'result-sub'}, title));
    if (sub) copy.append(el('span', {class: 'result-sub'}, sub));
    head.append(copy);
    return head;
}

function downloadBtn(name, data, mime) {
    const url = URL.createObjectURL(new Blob([data], {type: mime || 'application/octet-stream'}));
    const a = el('a', {href: url, download: name, html: icon('download')});
    a.append(el('span', {text: t('download')}));
    a.addEventListener('click', () => setTimeout(() => URL.revokeObjectURL(url), 60000));
    return a;
}

async function renderOpResult(res, sourceName) {
    const tool = currentTool();
    const toolId = state.tool;
    const box = el('div');

    if (toolId === 'totext' && res.text !== undefined) {
        const head = resultHead(true);
        const ta = el('textarea', {class: 'text-out', spellcheck: 'false'});
        ta.value = res.text || '';
        const copy = el('button', {class: 'ghost', type: 'button', html: icon('copy')});
        copy.append(el('span', {text: t('copy')}));
        copy.addEventListener('click', async () => {
            try {
                await navigator.clipboard.writeText(ta.value);
                copy.textContent = t('copied');
            } catch (err) {
                warn('clipboard', err);
                copy.textContent = t('failed');
            }
        });
        head.append(el('span', {class: 'result-actions'}, copy));
        box.append(head, kvTable({chars: res.chars, pages: res.pages}), ta);
        showResult(box);
        if (res.data && prefs.autoDl) download(resultFileName(res, sourceName), res.data, 'text/plain');
        return;
    }

    if (toolId === 'ocr' && res.imagesProcessed === 0) {
        box.append(resultHead(true, t('msg_ocr_none')));
        showResult(box);
        return;
    }

    if (toolId === 'replace' && res.occurrences === 0) {
        box.append(resultHead(true, t('msg_replace_none')));
        showResult(box);
        return;
    }

    if (res.files && res.files.length) {
        const head = resultHead(true, res.files.length + ' ' + t('d_files'));
        const actions = el('div', {class: 'result-actions'});
        const dl = el('a', {href: '#', html: icon('download')});
        dl.append(el('span', {text: t('downloadAll')}));
        dl.addEventListener('click', async (ev) => {
            ev.preventDefault();
            for (const f of res.files) {
                download(f.name, f.data);
                await new Promise((r) => setTimeout(r, 400));
            }
        });
        actions.append(dl);
        head.append(actions);
        box.append(head);
        const list = el('table', {class: 'list'});
        list.append(el('tr', {}, el('th', {text: t('d_files')}), el('th', {text: t('att_size')})));
        for (const f of res.files) list.append(el('tr', {}, el('td', {text: f.name}), el('td', {
            class: 'num',
            text: fmtSize(f.data.length)
        })));
        box.append(list);
        showResult(box);
        for (const f of res.files) addResultEntry(f.name, f.data);
        return;
    }

    if (res.report !== undefined) {
        box.append(resultHead(true, (res.count || 0) + ' 脳'), el('pre', {class: 'report'}, res.report));
        showResult(box);
        return;
    }

    if (res.attachments) {
        box.append(resultHead(true, res.count + ' ' + t('d_attachments')));
        if (!res.count) box.append(el('p', {class: 'empty'}, t('att_none')));
        else {
            const list = el('table', {class: 'list'});
            list.append(el('tr', {}, el('th', {text: t('att_name')}), el('th', {text: t('att_desc_col')})));
            for (const a of res.attachments) list.append(el('tr', {}, el('td', {text: a.name}), el('td', {text: a.desc || ''})));
            box.append(list);
        }
        showResult(box);
        return;
    }

    if (res.items) {
        const okCount = res.items.filter((i) => i.ok).length;
        const head = resultHead(true, t('batch_summary')(okCount, res.items.length));
        head.append(el('span', {class: 'result-actions'}, downloadBtn(res.name || 'batch.zip', res.data, 'application/zip')));
        box.append(head);
        const ul = el('div', {class: 'batch-status'});
        for (const it of res.items) {
            ul.append(el('div', {class: 'item'},
                el('span', {class: it.ok ? 'ok' : 'fail', html: icon(it.ok ? 'check' : 'error')}),
                el('span', {text: it.name + (it.error ? ' — ' + it.error : '')})));
        }
        box.append(ul);
        showResult(box);
        if (prefs.autoDl) download(res.name || 'batch.zip', res.data, 'application/zip');
        return;
    }

    if (res.data && res.data.length) {
        const name = resultFileName(res, sourceName);
        const head = resultHead(true, name);
        head.append(el('span', {class: 'result-actions'}, downloadBtn(name, res.data, /\.txt$/.test(name) ? 'text/plain' : 'application/pdf')));
        box.append(head, kvTable(res));

        if (/\.pdf$/i.test(name)) {
            const entry = addResultEntry(name, res.data, 'pdf');
            const strip = el('div', {class: 'thumb-strip'});
            const u = await thumbURL(entry, 1);
            if (u) {
                strip.append(el('img', {src: u, alt: name}));
                box.append(strip);
            }
            if (effectivePick(tool) === 'single') {
                state.activeId = entry.id;
                renderFiles();
            }
        }
        if (prefs.autoDl) download(name, res.data);
        showResult(box);
        return;
    }

    box.append(resultHead(true));
    const kv = kvTable(res);
    if (kv) box.append(kv);
    showResult(box);
}

async function runTool() {
    const tool = currentTool();
    const toolId = state.tool;
    const values = currentFormValues(tool);

    for (const [k, v] of Object.entries(values)) lastValues[fieldKey(toolId, k)] = v;
    saveLastValues();

    const pick = effectivePick(tool, values);
    let file = null, files = [];
    if (pick === 'multi') {
        files = state.multiIds.map(byId).filter(Boolean);
        if (!files.length) {
            notify(t('pickNone'), 'error');
            return;
        }
    } else {
        file = byId(state.activeId);
        if (!file) {
            notify(t('pickOne'), 'error');
            return;
        }
        if (toolId === 'attachments' && values.action === 'add') files = [file, ...state.attachmentDrafts];
    }

    const required = $('#toolForm').querySelectorAll('[required]');
    for (const input of required) {
        const conditional = input.closest('[data-when]');
        if ((!conditional || !conditional.hidden) && !input.value.trim()) {
            input.reportValidity();
            input.focus();
            return;
        }
    }

    let logo, logoName = 'logo';
    if (toolId === 'batch' && values.mode === 'image') {
        const le = byId(values.imageId);
        if (!le) {
            notify(t('f_wm_imageId'), 'error');
            return;
        }
        logo = le.data;
        logoName = le.name;
    }

    setBusy(true);
    $('#result').hidden = true;
    try {
        const res = await tool.exec(values, file, files, logo, logoName);
        if (!res || res.ok === false) throw new Error((res && res.error) || 'unknown error');
        await renderOpResult(res, file ? file.name : (files[0] && files[0].name));
        if (toolId === 'attachments' && values.action === 'add') {
            state.attachmentDrafts = [];
            renderToolTarget();
        }
    } catch (err) {
        console.error('[webui] ' + state.tool + ':', err);
        showResult(resultHead(false, err.message));
    } finally {
        setBusy(false);
    }
}

/* ============================= file grid ============================= */

function renderFiles() {
    const grid = $('#fileGrid');
    if (!grid) return;
    grid.textContent = '';
    const tool = currentTool();
    const values = currentFormValues(tool);
    const pick = effectivePick(tool, values);
    const count = $('#fileCount');
    if (count) count.textContent = state.files.length ? t('fileCount')(state.files.length) : t('noFilesShort');
    const selectAll = $('#selectAllBtn');
    if (selectAll) selectAll.hidden = pick !== 'multi' || !state.files.some((f) => isSelectable(f, tool, values));
    const clear = $('#clearFilesBtn');
    if (clear) clear.disabled = !state.files.length;

    if (!state.files.length) {
        grid.append(el('div', {class: 'empty'}, t('noFiles')));
        updateRunState();
        return;
    }

    for (const f of state.files) {
        const selectable = isSelectable(f, tool, values);
        const selected = pick === 'multi' ? state.multiIds.includes(f.id) : state.activeId === f.id;
        const card = el('div', {
            class: 'file-card' + (selected ? (pick === 'multi' ? ' checked' : ' selected') : '') + (selectable ? '' : ' disabled'),
            role: 'button', tabindex: selectable ? '0' : '-1', 'aria-pressed': selected ? 'true' : 'false',
            onClick: () => {
                if (!selectable) return;
                if (pick === 'multi') toggleMulti(f.id); else selectSingle(f.id);
            },
        });
        card.addEventListener('keydown', (ev) => {
            if (ev.key !== 'Enter' && ev.key !== ' ') return;
            ev.preventDefault();
            if (selectable) card.click();
        });

        const thumb = el('div', {class: 'file-thumb'});
        const placeholder = el('span', {
            class: 'file-placeholder',
            html: icon(f.kind === 'image' ? 'image' : (f.encrypted ? 'lock' : 'file'))
        });
        thumb.append(placeholder);
        card.append(thumb);
        const showThumb = (url) => {
            if (!url) return;
            placeholder.remove();
            thumb.classList.remove('loading');
            thumb.append(el('img', {src: url, alt: f.name, loading: 'lazy'}));
        };
        if (f.kind === 'image') {
            thumbURL(f).then(showThumb).catch((err) => warn('thumb ' + f.name, err));
        } else if (f.kind === 'pdf' && !f.encrypted && !f.probeErr && engine.ready) {
            thumb.classList.add('loading');
            thumbURL(f, 1).then((u) => {
                thumb.classList.remove('loading');
                if (u) showThumb(u);
            }).catch((err) => {
                thumb.classList.remove('loading');
                warn('thumb ' + f.name, err);
            });
        }

        const meta = el('div', {class: 'file-meta'});
        const name = el('div', {class: 'file-name', title: f.name}, f.name);
        if (f.isResult) name.append(el('span', {class: 'result-badge'}, t('resultFile')));
        meta.append(name);
        const sub = el('div', {class: 'file-sub'});
        sub.append(el('span', {text: fmtSize(f.size)}));
        if (f.kind === 'pdf' && f.pageCount) sub.append(el('span', {text: '· ' + f.pageCount + ' ' + t('pages')}));
        if (f.encrypted) sub.append(el('span', {class: 'lock', text: '· ' + t('encrypted')}));
        if (f.probeErr) sub.append(el('span', {class: 'probe-error', title: f.probeErr, text: '· ' + f.probeErr}));
        if (f.kind === 'image') sub.append(el('span', {text: '· ' + t('image')}));
        meta.append(sub);
        card.append(meta);
        if (selectable) card.append(el('span', {class: 'selection-check', html: icon('check')}));
        if (pick === 'multi' && selected) card.append(el('span', {class: 'file-order'}, String(state.multiIds.indexOf(f.id) + 1)));
        card.append(el('button', {
            class: 'file-del',
            title: t('remove'),
            'aria-label': t('remove') + ' ' + f.name,
            type: 'button',
            html: icon('close'),
            onClick: (ev) => {
                ev.stopPropagation();
                removeFile(f.id);
            },
        }));
        grid.append(card);
    }
    updateRunState();
}

/* ============================= navigation ============================= */

function renderNav() {
    const nav = $('#nav');
    nav.textContent = '';
    for (const [groupId, ids] of GROUPS) {
        nav.append(el('div', {class: 'nav-group'}, t('group' + groupId[0].toUpperCase() + groupId.slice(1))));
        for (const id of ids) {
            const tool = TOOLS[id];
            nav.append(el('button', {
                    class: 'nav-item' + (state.tool === id ? ' active' : ''),
                    type: 'button', 'aria-current': state.tool === id ? 'page' : null,
                    onClick: () => selectTool(id),
                },
                el('span', {class: 'ico', html: icon(tool.icon)}),
                el('span', {class: 'nav-label', text: t(tool.title)})));
        }
    }
}

function selectTool(id) {
    state.tool = id;
    const tool = currentTool();
    const active = byId(state.activeId);
    if (active && !isSelectable(active, tool, currentFormValues(tool))) state.activeId = null;
    state.multiIds = state.multiIds.filter((fid) => {
        const f = byId(fid);
        return f && isSelectable(f, tool, {});
    });
    $('#result').hidden = true;
    $('#result').textContent = '';
    document.body.classList.remove('nav-open');
    const backdrop = $('#navBackdrop');
    if (backdrop) backdrop.hidden = true;
    renderNav();
    renderToolChrome();
    renderToolForm();
    renderFiles();
    renderToolTarget();
    updateRunState();
}

function renderToolChrome() {
    const tool = currentTool();
    $('#toolTitle').textContent = t(tool.title);
    $('#toolDesc').textContent = t(tool.desc);
    const toolIcon = $('#toolIcon');
    if (toolIcon) toolIcon.innerHTML = icon(tool.icon);
    updateRunState();
}

/* ============================= preferences dialog ============================= */

function openPrefs() {
    $('#prefLang').value = prefs.lang;
    $('#prefTheme').value = prefs.theme;
    $('#prefDpi').value = String(prefs.dpi);
    $('#prefAutoDl').checked = !!prefs.autoDl;
    $('#prefsDialog').showModal();
}

function applyPrefsFromDialog() {
    prefs.lang = $('#prefLang').value;
    prefs.theme = $('#prefTheme').value;
    prefs.dpi = parseInt($('#prefDpi').value, 10) || 96;
    prefs.autoDl = $('#prefAutoDl').checked;
    savePrefs();
    applyTheme();
    $('#langSel').value = prefs.lang;
    applyStaticI18n();
    renderNav();
    renderToolChrome();
    renderToolForm();
    renderFiles();
    renderToolTarget();
}

/* ============================= GitHub support prompt ============================= */

const STAR_PROMPT_KEY = 'pdfcpu2.webui.starPromptSeen';

function openStarPrompt() {
    const dialog = $('#starDialog');
    if (!dialog) return;
    try {
        localStorage.setItem(STAR_PROMPT_KEY, '1');
    } catch (_) { /* storage is optional */
    }
    if (!dialog.open) dialog.showModal();
}

function initStarPrompt() {
    const button = $('#starBtn');
    const dialog = $('#starDialog');
    if (button) button.addEventListener('click', openStarPrompt);
    const link = $('#starModalLink');
    if (link) link.addEventListener('click', () => {
        try {
            localStorage.setItem(STAR_PROMPT_KEY, '1');
        } catch (_) { /* storage is optional */
        }
    });
    let seen = false;
    try {
        seen = localStorage.getItem(STAR_PROMPT_KEY) === '1';
    } catch (_) { /* storage is optional */
    }
    if (!seen && dialog) setTimeout(openStarPrompt, 1400);
}

/* ============================= boot ============================= */

function initDropzone() {
    const dz = $('#dropzone');
    const input = $('#fileInput');

    // The hidden input is a child of the dropzone: its own (bubbling) click
    // must not re-trigger input.click(), otherwise the handler recurses into
    // itself until the stack overflows and the picker never opens.
    dz.addEventListener('click', (ev) => {
        if (ev.target !== input) input.click();
    });
    dz.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter' || ev.key === ' ') {
            ev.preventDefault();
            input.click();
        }
    });
    input.addEventListener('change', () => {
        addFiles([...input.files]);
        input.value = '';
    });

    ['dragenter', 'dragover'].forEach((evName) => {
        dz.addEventListener(evName, (ev) => {
            ev.preventDefault();
            dz.classList.add('drag');
        });
    });
    ['dragleave', 'drop'].forEach((evName) => {
        dz.addEventListener(evName, (ev) => {
            ev.preventDefault();
            dz.classList.remove('drag');
        });
    });
    dz.addEventListener('drop', (ev) => {
        ev.preventDefault();
        const files = [...(ev.dataTransfer ? ev.dataTransfer.files : [])];
        if (files.length) addFiles(files);
    });

    window.addEventListener('dragover', (ev) => ev.preventDefault());
    window.addEventListener('drop', (ev) => ev.preventDefault());
}

function init() {
    applyTheme();
    $('#langSel').value = prefs.lang;
    applyStaticI18n();
    document.title = t('appTitle');

    engine.init();
    initDropzone();
    initStarPrompt();

    $('#langSel').addEventListener('change', (ev) => {
        prefs.lang = ev.target.value;
        savePrefs();
        applyStaticI18n();
        renderNav();
        renderToolChrome();
        renderToolForm();
        renderFiles();
        renderToolTarget();
    });
    $('#themeBtn').addEventListener('click', () => {
        prefs.theme = prefs.theme === 'dark' ? 'light' : 'dark';
        savePrefs();
        applyTheme();
    });
    $('#prefsBtn').addEventListener('click', openPrefs);
    $('#prefsDialog').addEventListener('close', () => {
        if ($('#prefsDialog').returnValue === 'ok') applyPrefsFromDialog();
    });
    $('#clearFilesBtn').addEventListener('click', () => {
        state.files = [];
        state.activeId = null;
        state.multiIds = [];
        state.attachmentDrafts = [];
        thumbCache.forEach((u) => URL.revokeObjectURL(u));
        thumbCache.clear();
        renderFiles();
        renderToolTarget();
    });
    $('#runBtn').addEventListener('click', runTool);
    $('#toolForm').addEventListener('submit', (ev) => {
        ev.preventDefault();
        runTool();
    });

    const attachmentInput = $('#attachmentInput');
    if (attachmentInput) attachmentInput.addEventListener('change', async () => {
        await addAttachmentFiles([...attachmentInput.files]);
        attachmentInput.value = '';
    });
    const selectAll = $('#selectAllBtn');
    if (selectAll) selectAll.addEventListener('click', () => {
        const tool = currentTool();
        const values = currentFormValues(tool);
        state.multiIds = state.files.filter((f) => isSelectable(f, tool, values)).map((f) => f.id);
        renderFiles();
        renderToolTarget();
        updateRunState();
    });
    const navToggle = $('#navToggle');
    const backdrop = $('#navBackdrop');
    const closeNav = () => {
        document.body.classList.remove('nav-open');
        if (backdrop) backdrop.hidden = true;
        if (navToggle) navToggle.setAttribute('aria-expanded', 'false');
    };
    if (navToggle) navToggle.addEventListener('click', () => {
        const open = document.body.classList.toggle('nav-open');
        if (backdrop) backdrop.hidden = !open;
        navToggle.setAttribute('aria-expanded', String(open));
    });
    if (backdrop) backdrop.addEventListener('click', closeNav);
    document.addEventListener('keydown', (ev) => {
        if (ev.key === 'Escape') closeNav();
    });
    if (window.matchMedia) window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
        if (prefs.theme === 'system') applyTheme();
    });

    renderNav();
    selectTool('encrypt');
    renderEngineStatus();
}

document.addEventListener('DOMContentLoaded', init);

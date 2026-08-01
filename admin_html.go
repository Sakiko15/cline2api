package main

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Cline 代理管理面板</title>
<style>
:root{
  --bg:#f5f5f7;--surface:#ffffff;--surface2:#fbfbfd;
  --border:#d2d2d7;--border2:#e8e8ed;
  --text:#1d1d1f;--text2:#86868b;--text3:#aeaeb2;
  --accent:#007aff;--accent-hover:#0066d6;--accent-soft:#e8f0ff;
  --green:#34c759;--green-soft:#e8f9ed;
  --red:#ff3b30;--red-soft:#ffefee;
  --yellow:#ff9500;--yellow-soft:#fff5e6;
  --shadow-sm:0 1px 2px rgba(0,0,0,0.04),0 1px 3px rgba(0,0,0,0.06);
  --shadow-md:0 4px 12px rgba(0,0,0,0.06),0 1px 3px rgba(0,0,0,0.04);
  --shadow-lg:0 12px 32px rgba(0,0,0,0.08),0 2px 8px rgba(0,0,0,0.04);
  --radius:16px;--radius-sm:10px;--radius-xs:8px;
  --ease:cubic-bezier(0.4,0,0.2,1);
  --blue:#007aff;
}
*{margin:0;padding:0;box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{font-family:-apple-system,BlinkMacSystemFont,'SF Pro Display','SF Pro Text','Segoe UI','Noto Sans',Helvetica,Arial,sans-serif;background:var(--bg);color:var(--text);font-size:15px;line-height:1.5;-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
.layout{display:flex;min-height:100vh}

/* ===== Sidebar ===== */
.sidebar{width:260px;background:var(--surface);border-right:1px solid var(--border2);padding:0;flex-shrink:0;display:flex;flex-direction:column;position:sticky;top:0;height:100vh}
.sidebar-header{padding:20px 20px 16px;border-bottom:1px solid var(--border2)}
.sidebar-header .brand{display:flex;align-items:center;gap:10px}
.sidebar-header .logo{width:32px;height:32px;border-radius:8px;background:linear-gradient(135deg,#007aff,#5856d6);display:flex;align-items:center;justify-content:center;flex-shrink:0;box-shadow:var(--shadow-sm)}
.sidebar-header .logo svg{width:18px;height:18px;color:#fff}
.sidebar-header .brand-name{font-size:15px;font-weight:600;color:var(--text);letter-spacing:-0.2px}
.sidebar-header .brand-sub{font-size:12px;color:var(--text2);margin-top:2px}
.nav-section{padding:16px 12px 8px}
.nav-section-label{font-size:11px;font-weight:600;color:var(--text3);text-transform:uppercase;letter-spacing:0.5px;padding:0 8px 8px}
.nav-item{display:flex;align-items:center;gap:12px;padding:9px 12px;cursor:pointer;color:var(--text2);transition:all 0.18s var(--ease);border-radius:var(--radius-xs);margin-bottom:2px;font-size:14px;font-weight:500}
.nav-item:hover{color:var(--text);background:var(--surface2)}
.nav-item.active{color:var(--accent);background:var(--accent-soft)}
.nav-item svg{width:20px;height:20px;flex-shrink:0}
.nav-item .nav-label{flex:1}
.sidebar-footer{margin-top:auto;padding:16px 20px;border-top:1px solid var(--border2);font-size:12px;color:var(--text2)}
.sidebar-footer a{color:var(--accent);text-decoration:none}
.sidebar-footer a:hover{text-decoration:underline}

/* ===== Main ===== */
.main{flex:1;min-width:0;padding:36px clamp(24px,4vw,64px) 64px;overflow-y:auto}
.content-shell{width:min(100%,1440px);margin:0 auto}
.large-title{font-size:32px;font-weight:700;letter-spacing:0;color:var(--text);margin-bottom:4px;text-wrap:pretty}
.large-subtitle{font-size:15px;color:var(--text2);margin-bottom:28px}
.page-header{display:flex;justify-content:space-between;align-items:flex-end;gap:20px;margin-bottom:28px}
.page-header h2{font-size:28px;font-weight:700;letter-spacing:0}

/* ===== Dashboard ===== */
.metric-section{margin-bottom:26px}
.metric-heading{font-size:13px;font-weight:600;color:var(--text2);margin:0 0 10px 2px}
.cards{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:14px}
.cards.tokens{grid-template-columns:repeat(4,minmax(0,1fr))}
.card{min-width:0;background:var(--surface);border:1px solid var(--border2);border-radius:12px;padding:18px;box-shadow:var(--shadow-sm);transition:transform 0.2s var(--ease),box-shadow 0.2s var(--ease)}
.card:hover{transform:translateY(-1px);box-shadow:var(--shadow-md)}
.card .card-icon{width:34px;height:34px;border-radius:10px;display:flex;align-items:center;justify-content:center;margin-bottom:12px}
.card .card-icon svg{width:18px;height:18px}
.card .num{overflow:hidden;text-overflow:ellipsis;font-size:30px;font-weight:700;letter-spacing:0;line-height:1.1;font-variant-numeric:tabular-nums}
.card .label{font-size:13px;color:var(--text2);margin-top:5px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.card.blue .card-icon{background:var(--accent-soft)}.card.blue .card-icon svg{color:var(--blue)}.card.blue .num{color:var(--blue)}
.card.green .card-icon{background:var(--green-soft)}.card.green .card-icon svg{color:var(--green)}.card.green .num{color:var(--green)}
.card.yellow .card-icon{background:var(--yellow-soft)}.card.yellow .card-icon svg{color:var(--yellow)}.card.yellow .num{color:var(--yellow)}
.card.red .card-icon{background:var(--red-soft)}.card.red .card-icon svg{color:var(--red)}.card.red .num{color:var(--red)}

/* ===== Section / Grouped ===== */
.section{background:var(--surface);border:1px solid var(--border2);border-radius:var(--radius);margin-bottom:20px;overflow:hidden;box-shadow:var(--shadow-sm)}
.section-title{padding:16px 20px 8px;font-weight:600;font-size:15px;display:flex;align-items:center;gap:8px}
.section-title svg{width:18px;height:18px;color:var(--text2)}
.section-desc{padding:0 20px 12px;font-size:13px;color:var(--text2)}
.section-body{padding:16px 20px}
.section-body.flush{padding:0}

/* ===== Tabs (segmented) ===== */
.tabs{display:flex;border-bottom:1px solid var(--border2);padding:0 20px;gap:4px}
.tab{padding:12px 18px;cursor:pointer;color:var(--text2);border-bottom:2px solid transparent;font-size:14px;font-weight:500;transition:all 0.18s var(--ease)}
.tab:hover{color:var(--text)}
.tab.active{color:var(--accent);border-bottom-color:var(--accent)}
.tab-content{display:none;padding:20px}
.tab-content.active{display:block}

/* ===== Table ===== */
table{width:100%;border-collapse:collapse;table-layout:fixed}
th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--border2);font-size:13px;vertical-align:middle}
.section-body.flush{overflow-x:auto}
th{color:var(--text2);font-weight:600;font-size:11px;text-transform:uppercase;letter-spacing:0.4px;white-space:nowrap}
tbody tr:last-child td{border-bottom:none}
tbody tr{transition:background 0.15s var(--ease)}
tbody tr:hover{background:var(--surface2)}
.account-table th:first-child,.account-table td:first-child{width:16%}
.account-table th:nth-child(2),.account-table td:nth-child(2){width:8%}
.account-table th:nth-child(3),.account-table td:nth-child(3){width:5%}
.account-table th:nth-child(4),.account-table td:nth-child(4),.account-table th:nth-child(5),.account-table td:nth-child(5),.account-table th:nth-child(6),.account-table td:nth-child(6),.account-table th:nth-child(7),.account-table td:nth-child(7){width:7%;text-align:right;font-variant-numeric:tabular-nums}
.account-table th:nth-child(8),.account-table td:nth-child(8),.account-table th:nth-child(9),.account-table td:nth-child(9){width:11%;white-space:nowrap;color:var(--text2)}
.account-table th:last-child,.account-table td:last-child{width:120px;min-width:120px;text-align:right;white-space:nowrap}
.account-table td:last-child .btn{width:32px;padding-left:0;padding-right:0;justify-content:center}
.account-email{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text);font-weight:500}
.account-cards{display:none}
.log-cards{display:none}
.log-table th:first-child,.log-table td:first-child{width:14%;white-space:nowrap;color:var(--text2)}
.log-table th:nth-child(2),.log-table td:nth-child(2){width:16%}
.log-table th:nth-child(3),.log-table td:nth-child(3){width:7%}
.log-table th:nth-child(4),.log-table td:nth-child(4){width:15%}
.log-table th:nth-child(5),.log-table td:nth-child(5),.log-table th:nth-child(6),.log-table td:nth-child(6),.log-table th:nth-child(7),.log-table td:nth-child(7),.log-table th:nth-child(8),.log-table td:nth-child(8){width:6%;text-align:right;font-variant-numeric:tabular-nums}
.log-table th:nth-child(9),.log-table td:nth-child(9),.log-table th:nth-child(10),.log-table td:nth-child(10),.log-table th:nth-child(11),.log-table td:nth-child(11){width:6%;text-align:right;font-variant-numeric:tabular-nums}
.log-table th:last-child,.log-table td:last-child{width:6%;text-align:right;white-space:nowrap}
.log-status{display:inline-flex;align-items:center;gap:5px;padding:3px 10px;border-radius:12px;font-size:11px;font-weight:600}
.log-status.ok{background:var(--green-soft);color:var(--green)}
.log-status.fail{background:var(--red-soft);color:var(--red)}

/* ===== Status badges ===== */
.status{display:inline-flex;align-items:center;gap:5px;padding:3px 10px;border-radius:12px;font-size:12px;font-weight:600}
.status.active{background:var(--green-soft);color:var(--green)}
.status.cooldown{background:var(--yellow-soft);color:var(--yellow)}
.status.expired{background:var(--red-soft);color:var(--red)}
.status-dot{width:6px;height:6px;border-radius:50%;display:inline-block}
.status-dot.active{background:var(--green)}
.status-dot.cooldown{background:var(--yellow)}
.status-dot.expired{background:var(--red)}

/* ===== Buttons ===== */
.btn{display:inline-flex;align-items:center;gap:6px;padding:8px 16px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--surface);color:var(--text);cursor:pointer;font-size:14px;font-weight:500;transition:all 0.18s var(--ease);text-decoration:none;line-height:1.2;white-space:nowrap}
.btn:hover{background:var(--surface2);border-color:var(--text3)}
.btn:active{transform:scale(0.97)}
.btn svg{width:16px;height:16px}
.btn-primary{background:var(--accent);border-color:var(--accent);color:#fff}
.btn-primary:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
.btn-success{background:var(--green);border-color:var(--green);color:#fff}
.btn-success:hover{background:#2bb24c;border-color:#2bb24c}
.btn-danger{border-color:var(--red);color:var(--red);background:var(--surface)}
.btn-danger:hover{background:var(--red);color:#fff}
.btn-sm{padding:5px 12px;font-size:13px}
.btn-icon{padding:6px;width:32px;height:32px;justify-content:center}

/* ===== Inputs ===== */
input,textarea,select{width:100%;padding:10px 14px;background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text);font-size:14px;font-family:inherit;transition:border-color 0.18s var(--ease),box-shadow 0.18s var(--ease)}
input:focus,textarea:focus,select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-soft)}
input:disabled{background:var(--surface2);color:var(--text2)}
textarea{resize:vertical;min-height:88px;font-family:ui-monospace,'SF Mono','Cascadia Code','Consolas',monospace;font-size:12px;line-height:1.6}
::placeholder{color:var(--text3)}

/* ===== Forms ===== */
.form-row{display:flex;gap:14px;align-items:flex-end;margin-bottom:14px}
.form-row .field{flex:1}
.form-row .field label{display:block;font-size:13px;color:var(--text2);margin-bottom:6px;font-weight:500}
.form-actions{display:flex;gap:8px;margin-top:14px}

/* ===== Toast ===== */
.toast{position:fixed;top:24px;left:50%;transform:translateX(-50%) translateY(-20px);padding:12px 20px;border-radius:var(--radius-sm);color:#fff;z-index:9999;opacity:0;transition:all 0.3s var(--ease);font-size:14px;font-weight:500;max-width:420px;box-shadow:var(--shadow-lg);display:flex;align-items:center;gap:8px}
.toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
.toast.success{background:var(--green)}
.toast.error{background:var(--red)}
.toast.info{background:var(--accent)}

/* ===== Loading ===== */
.loading{display:inline-block;width:14px;height:14px;border:2px solid var(--text3);border-top-color:var(--accent);border-radius:50%;animation:spin 0.7s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}

/* ===== Misc ===== */
.empty{padding:40px;text-align:center;color:var(--text2);font-size:14px}
.empty a{color:var(--accent);cursor:pointer;text-decoration:none}
.empty a:hover{text-decoration:underline}
.mono{font-family:ui-monospace,'SF Mono','Cascadia Code','Consolas',monospace;font-size:12px}
.flex{display:flex;align-items:center;gap:8px}
.justify-between{display:flex;justify-content:space-between;align-items:center}
.text-right{text-align:right}
.mt-8{margin-top:8px}
.inline-flex{display:inline-flex;align-items:center;gap:6px}

.key-display{background:var(--surface2);padding:10px 14px;border-radius:var(--radius-sm);border:1px solid var(--border2);font-family:ui-monospace,'SF Mono','Cascadia Code','Consolas',monospace;font-size:12px;word-break:break-all;cursor:pointer;transition:all 0.15s var(--ease);color:var(--text)}
.key-display:hover{background:var(--accent-soft);border-color:var(--accent)}
.copy-icon{cursor:pointer;color:var(--text2);padding:2px 6px;border-radius:4px}
.copy-icon:hover{color:var(--text);background:var(--surface2)}

.empty-state{padding:48px 24px;text-align:center;color:var(--text2)}
.empty-state .icon{width:48px;height:48px;margin:0 auto 14px;display:flex;align-items:center;justify-content:center;border-radius:14px;background:var(--surface2);color:var(--text3)}
.empty-state .icon svg{width:24px;height:24px}

.model-tag{display:inline-block;padding:4px 10px;border-radius:8px;font-size:12px;font-weight:500;background:var(--surface2);color:var(--text2);margin:3px;border:1px solid var(--border2)}
.model-tag.free{border-color:var(--green);color:var(--green);background:var(--green-soft)}
.model-tag.pass{border-color:var(--yellow);color:var(--yellow);background:var(--yellow-soft)}

/* action row */
.action-row{display:flex;gap:8px;flex-wrap:wrap}

/* ===== Responsive ===== */
@media (max-width:760px){
  .layout{display:block}
  .sidebar{width:100%;height:auto;min-height:0;position:static;border-right:none;border-bottom:1px solid var(--border2)}
  .sidebar-header{padding:14px 16px;border-bottom:none}
  .sidebar-header .brand-sub,.nav-section-label,.sidebar-footer{display:none}
  .nav-section{display:flex;padding:0 10px 12px;gap:4px;overflow-x:auto}
  .nav-item{flex:1;justify-content:center;gap:6px;margin:0;padding:8px 10px;min-width:max-content}
  .nav-item svg{width:18px;height:18px}
	  .main{padding:24px 16px 40px;max-width:none}
	  .content-shell{width:100%}
	  .large-title{font-size:28px;letter-spacing:0}
  .large-subtitle{margin-bottom:22px;font-size:14px}
  .page-header{align-items:flex-start;gap:14px;flex-direction:column;margin-bottom:22px}
	  .cards,.cards.tokens{grid-template-columns:repeat(2,minmax(0,1fr));gap:10px;margin-bottom:20px}
	  .card{padding:16px}
  .card .num{font-size:28px}
  .section{border-radius:14px;margin-bottom:16px}
  .section-title{padding:14px 16px 7px}
  .section-desc{padding:0 16px 10px}
  .section-body,.tab-content{padding:16px}
  .tabs{padding:0 12px;overflow-x:auto;white-space:nowrap}
  .tab{padding:11px 12px;font-size:13px}
  .form-row{flex-direction:column;align-items:stretch;gap:0;margin-bottom:0}
  .form-row .field{margin-bottom:14px}
  .form-actions,.action-row{gap:8px}
	  table{min-width:680px}
  .section-body.flush{overflow-x:auto}
  th,td{padding:11px 12px}
	  .toast{max-width:calc(100vw - 32px);width:max-content;text-align:center}
	}
	@media (min-width:761px) and (max-width:1180px){
	  .main{padding:30px 32px 56px}
	  .cards{grid-template-columns:repeat(2,minmax(0,1fr))}
	  .cards.tokens{grid-template-columns:repeat(3,minmax(0,1fr))}
	  .account-table th:nth-child(9),.account-table td:nth-child(9){display:none}
	}
	@media (max-width:760px){
	  .log-table{display:none}
	  .log-cards{display:grid;gap:10px;padding:12px}
	}
	@media (max-width:760px){
	  .account-table{display:none}
	  .account-cards{display:grid;gap:10px;padding:12px}
	  .account-card{border:1px solid var(--border2);border-radius:12px;padding:14px;background:var(--surface2)}
	  .account-card-header{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:12px}
	  .account-card .account-email{max-width:calc(100vw - 170px)}
	  .account-metrics{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;margin-bottom:12px}
	  .account-metric{padding:8px 10px;border-radius:8px;background:var(--surface)}
	  .account-metric-label{display:block;color:var(--text2);font-size:11px;margin-bottom:2px}
	  .account-metric-value{font-size:14px;font-weight:600;font-variant-numeric:tabular-nums}
	  .account-card-footer{display:flex;align-items:center;justify-content:space-between;gap:12px;color:var(--text2);font-size:11px}
	  .account-card-actions{display:flex;gap:6px;flex-shrink:0}
	  .section-body.flush{overflow:visible}
	}
	@media (max-width:390px){
	  .cards,.cards.tokens{grid-template-columns:1fr}
	  .nav-item .nav-label{font-size:12px}
	  .action-row .btn{flex:1;justify-content:center}
	}
</style>
</head>
<body>
<div class="layout">
<div class="sidebar">
  <div class="sidebar-header">
    <div class="brand">
      <div class="logo"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/></svg></div>
      <div>
        <div class="brand-name">Cline 代理</div>
        <div class="brand-sub">多账号轮询 · 双协议</div>
      </div>
    </div>
  </div>
  <div class="nav-section">
    <div class="nav-section-label">管理</div>
    <div class="nav-item active" data-tab="dashboard">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/></svg>
      <span class="nav-label">仪表盘</span>
    </div>
    <div class="nav-item" data-tab="accounts">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>
      <span class="nav-label">账号管理</span>
    </div>
    <div class="nav-item" data-tab="import">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
      <span class="nav-label">导入账号</span>
    </div>
    <div class="nav-item" data-tab="logs">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="8" y1="13" x2="16" y2="13"/><line x1="8" y1="17" x2="16" y2="17"/></svg>
      <span class="nav-label">请求日志</span>
    </div>
    <div class="nav-item" data-tab="settings">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
      <span class="nav-label">设置</span>
    </div>
  </div>
  <div class="sidebar-footer">
    <div>管理面板: <a href="/admin/">/admin/</a></div>
    <div>API 地址: <span id="footerApiAddr">http://127.0.0.1:3457</span></div>
  </div>
</div>

<div class="main">
<div class="content-shell">

<div id="tab-dashboard" class="tab-panel">
  <div class="large-title">仪表盘</div>
  <div class="large-subtitle">查看账号池状态与快捷操作</div>
  <div class="metric-section">
    <div class="metric-heading">账号状态</div>
    <div class="cards">
      <div class="card blue">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/></svg></div>
      <div class="num" id="statTotal">-</div><div class="label">账号总数</div>
    </div>
    <div class="card green">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg></div>
      <div class="num" id="statActive">-</div><div class="label">活跃</div>
    </div>
    <div class="card yellow">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg></div>
      <div class="num" id="statCooldown">-</div><div class="label">冷却</div>
    </div>
    <div class="card red">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/></svg></div>
      <div class="num" id="statExpired">-</div><div class="label">已过期</div>
    </div>
  </div>
  </div>
  <div class="metric-section">
    <div class="metric-heading">Token 用量</div>
    <div class="cards tokens">
    <div class="card blue">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v20M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg></div>
      <div class="num" id="statPromptTokens">-</div><div class="label">累计输入 Token</div>
    </div>
    <div class="card green">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v20M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg></div>
      <div class="num" id="statCompletionTokens">-</div><div class="label">累计输出 Token</div>
    </div>
    <div class="card yellow">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v20M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg></div>
      <div class="num" id="statTotalTokens">-</div><div class="label">累计总 Token</div>
    </div>
    <div class="card blue">
      <div class="card-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a10 10 0 1 0 10 10"/><path d="M12 6v6l4 2"/></svg></div>
      <div class="num" id="statCachedTokens">-</div><div class="label">缓存 Token</div>
    </div>
    </div>
  </div>
  <div class="section">
    <div class="section-title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/></svg>快捷操作</div>
    <div class="section-body action-row">
      <button class="btn btn-primary" onclick="switchTab('import')"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>添加账号</button>
      <button class="btn" onclick="refreshAllTokens()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>刷新全部 Token</button>
      <button class="btn" onclick="document.getElementById('fileInput').click()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>从文件导入</button>
      <input type="file" id="fileInput" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
      <button class="btn" onclick="switchTab('settings');generateKey()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg>生成 API 密钥</button>
    </div>
  </div>
</div>

<div id="tab-accounts" class="tab-panel" style="display:none">
  <div class="page-header">
    <div>
      <div class="large-title">账号管理</div>
      <div class="large-subtitle">管理 Cline 账号池中的所有账号</div>
    </div>
    <div style="display:flex;gap:8px">
      <button class="btn btn-sm" onclick="testAllAccounts(this)"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/></svg>测试全部</button>
      <button class="btn btn-sm" onclick="exportAccounts()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>导出</button>
      <button class="btn btn-primary btn-sm" onclick="switchTab('import')"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>添加</button>
      <button class="btn btn-sm" onclick="loadAccounts()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>刷新</button>
    </div>
  </div>
  <div class="section">
    <div class="section-body flush">
      <table class="account-table">
        <thead>
          <tr><th>邮箱</th><th>状态</th><th>请求</th><th>输入</th><th>输出</th><th>总 Token</th><th>缓存</th><th>最后使用</th><th>创建时间</th><th>操作</th></tr>
        </thead>
        <tbody id="accountTableBody">
          <tr><td colspan="10" class="empty">加载中...</td></tr>
        </tbody>
      </table>
      <div id="accountCards" class="account-cards"></div>
    </div>
  </div>
</div>

<div id="tab-import" class="tab-panel" style="display:none">
  <div class="large-title">导入账号</div>
  <div class="large-subtitle">通过 OAuth 登录、手动 Token 或批量文件添加账号</div>
  <div class="section">
    <div class="tabs" id="importTabs">
      <div class="tab active" data-tab="oauth">OAuth 浏览器登录</div>
      <div class="tab" data-tab="token">手动输入 Token</div>
      <div class="tab" data-tab="batch">批量导入</div>
    </div>

    <div id="import-oauth" class="tab-content active">
      <p style="color:var(--text2);margin-bottom:16px">通过浏览器完成 OAuth 认证，支持 Google/GitHub/邮箱登录，自动获取 refreshToken。</p>
      <button class="btn btn-primary" onclick="startOAuth()" id="oauthBtn"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M12 5l7 7-7 7"/></svg>开始 OAuth 登录</button>
      <div id="oauthProgress" style="display:none;margin-top:16px">
        <div style="display:flex;align-items:center;gap:12px">
          <div class="loading"></div>
          <div>
            <div style="font-weight:600" id="oauthStatus">等待浏览器授权...</div>
            <div style="color:var(--text2);font-size:13px;margin-top:4px">
              点击链接（系统浏览器打开）: <a href="#" id="oauthUrl" style="color:var(--accent);cursor:pointer"></a><br>
              并输入代码: <strong id="oauthUserCode"></strong>
            </div>
          </div>
        </div>
      </div>
      <div id="oauthResult" style="display:none;margin-top:16px"></div>
    </div>

    <div id="import-token" class="tab-content">
      <p style="color:var(--text2);margin-bottom:16px">输入已有的 Cline refreshToken，系统会自动验证并加入池。</p>
      <div class="form-row">
        <div class="field">
          <label>Refresh Token *</label>
          <input type="text" id="tokenInput" placeholder="粘贴 refreshToken">
        </div>
      </div>
      <div class="form-row">
        <div class="field">
          <label>邮箱（可选，留空自动生成）</label>
          <input type="text" id="tokenEmail" placeholder="user@example.com">
        </div>
      </div>
      <div class="form-actions">
        <button class="btn btn-primary" onclick="addByToken()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>添加账号</button>
      </div>
      <div id="tokenResult" style="margin-top:8px"></div>
    </div>

    <div id="import-batch" class="tab-content">
      <p style="color:var(--text2);margin-bottom:16px">批量导入多个账号。支持 JSON 数组或每行一个 token。</p>
      <div class="form-row">
        <div class="field">
          <label>JSON 数组格式：[{"refreshToken":"...","email":"..."}]</label>
          <textarea id="batchInput" placeholder='[{"refreshToken":"xxx","email":"u1@x.com"},{"refreshToken":"yyy","email":"u2@x.com"}]'></textarea>
        </div>
      </div>
      <div class="form-actions">
        <button class="btn btn-primary" onclick="batchImport()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>导入全部</button>
        <button class="btn" onclick="document.getElementById('fileInput2').click()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>选择文件</button>
        <input type="file" id="fileInput2" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
      </div>
      <div id="batchResult" style="margin-top:8px"></div>
    </div>
  </div>
</div>

<div id="tab-logs" class="tab-panel" style="display:none">
  <div class="page-header">
    <div>
      <div class="large-title">请求日志</div>
      <div class="large-subtitle">查看每次请求的 Token 消耗、缓存、耗时与流式速度</div>
    </div>
    <div style="display:flex;gap:8px">
      <button class="btn btn-sm" onclick="loadRequestLogs(true)"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>刷新</button>
    </div>
  </div>
  <div class="section">
    <div class="section-body flush">
      <table class="log-table">
        <thead>
          <tr><th>时间</th><th>账号</th><th>协议</th><th>模型</th><th>输入</th><th>输出</th><th>缓存</th><th>总</th><th>耗时</th><th>TTFT</th><th>tok/s</th><th>状态</th></tr>
        </thead>
        <tbody id="logTableBody">
          <tr><td colspan="12" class="empty">加载中...</td></tr>
        </tbody>
      </table>
      <div id="logCards" class="log-cards"></div>
    </div>
  </div>
  <div id="logLoadMore" style="display:none;text-align:center;padding:16px">
    <button class="btn btn-primary" onclick="loadRequestLogs(false)">加载更多</button>
  </div>
</div>

<div id="tab-settings" class="tab-panel" style="display:none">
  <div class="large-title">设置</div>
  <div class="large-subtitle">管理 API 密钥、模型、代理配置与请求头</div>

  <div class="section">
    <div class="section-title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg>API 密钥管理</div>
    <div class="section-desc">生成的密钥可用于客户端访问代理 API（作为 x-api-key 或 Authorization 头）。</div>
    <div class="section-body">
      <div class="form-actions" style="margin-bottom:14px">
        <button class="btn btn-success" onclick="generateKey()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>生成新密钥</button>
      </div>
      <div id="keysList"></div>
      <div id="keyGenResult" style="margin-top:8px"></div>
    </div>
  </div>

  <div class="section">
    <div class="section-title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"/><line x1="9" y1="9" x2="15" y2="9"/><line x1="9" y1="15" x2="15" y2="15"/></svg>可用模型</div>
    <div class="section-body">
      <div id="modelsList" class="action-row">加载中...</div>
    </div>
  </div>

  <div class="section">
    <div class="section-title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>代理配置</div>
    <div class="section-body">
      <div class="form-row">
        <div class="field"><label>监听地址</label><input type="text" id="settingAddr" disabled></div>
        <div class="field"><label>默认模型</label><input type="text" id="settingDefModel" disabled></div>
      </div>
      <div class="form-row">
        <div class="field">
          <label>轮询策略</label>
          <select id="settingStrategy" onchange="updateConfig()">
            <option value="round_robin">轮询 (round_robin)</option>
            <option value="fill">填满 (fill)</option>
            <option value="random">随机 (random)</option>
          </select>
        </div>
        <div class="field"><label>引擎版本</label><input type="text" id="settingVersion" disabled></div>
      </div>
      <div class="form-row">
        <div class="field"><label>账号文件</label><input type="text" id="settingPoolPath" disabled></div>
      </div>
    </div>
  </div>

  <div class="section">
    <div class="section-title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7V4h16v3"/><path d="M9 20h6"/><path d="M12 4v16"/></svg>请求头配置（模拟 Cline CLI 发出）</div>
    <div class="section-desc">这些请求头会附加到所有转发给 Cline API 的请求中，以模拟官方客户端行为。</div>
    <div class="section-body">
      <table>
        <thead><tr><th style="width:240px">请求头</th><th>值</th><th style="width:48px"></th></tr></thead>
        <tbody id="headersTableBody">
          <tr><td colspan="3" class="empty">加载中...</td></tr>
        </tbody>
      </table>
      <div class="form-actions" style="margin-top:14px">
        <button class="btn btn-sm" onclick="addHeaderRow()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>添加请求头</button>
        <button class="btn btn-sm btn-primary" onclick="saveHeaders()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><polyline points="17 21 17 13 7 13 7 21"/><polyline points="7 3 7 8 15 8"/></svg>保存请求头</button>
      </div>
      <div id="headerSaveResult" style="margin-top:8px"></div>
    </div>
  </div>

  <div class="section">
    <div class="section-title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>危险操作</div>
    <div class="section-body">
      <div class="action-row">
        <button class="btn btn-danger" onclick="deleteAllAccounts()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>删除全部账号</button>
        <button class="btn btn-danger" onclick="deleteAllKeys()"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>删除全部密钥</button>
      </div>
    </div>
  </div>
</div>

</div>
</div>
</div>

<div id="toast" class="toast"></div>

<script>
const API = '/admin/api';

const _ = id => document.getElementById(id);
const esc = s => { const d=document.createElement('div'); d.textContent=s||''; return d.innerHTML; };
const formatNumber = n => new Intl.NumberFormat('zh-CN').format(n || 0);
const formatTokenCount = n => {
  const value = Number(n) || 0;
  if (value < 1000) return String(value);
  const units = [['B', 1e9], ['M', 1e6], ['K', 1e3]];
  for (const [unit, size] of units) {
    if (value >= size) return (value / size).toFixed(1).replace(/\.0$/, '') + unit;
  }
  return String(value);
};

function toast(msg, t) {
  const el = _('toast');
  el.textContent = msg;
  el.className = 'toast ' + t + ' show';
  setTimeout(() => el.classList.remove('show'), 3500);
}

// 格式化冷却倒计时：传入 ISO 时间，返回 "58分钟后" 或 "已到期"
function formatCooldown(isoTime) {
  const until = new Date(isoTime);
  const diff = until - new Date();
  if (diff <= 0) return '即将恢复';
  const mins = Math.floor(diff / 60000);
  if (mins < 60) return mins + '分钟后';
  const hours = Math.floor(mins / 60);
  const remMin = mins % 60;
  return hours + '小时' + (remMin > 0 ? remMin + '分' : '') + '后';
}

// ========== 导航 ==========
document.querySelectorAll('.nav-item').forEach(el => {
  el.addEventListener('click', () => {
    if (el.classList.contains('active')) return;
    document.querySelectorAll('.nav-item').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
    _('tab-' + el.dataset.tab).style.display = 'block';
    if (el.dataset.tab === 'dashboard') { loadStats(); loadAccounts(); }
    if (el.dataset.tab === 'accounts') loadAccounts();
    if (el.dataset.tab === 'logs') loadRequestLogs(true);
    if (el.dataset.tab === 'settings') { loadKeys(); loadModels(); loadConfig(); }
  });
});

function switchTab(name) {
  document.querySelectorAll('.nav-item').forEach(e => {
    e.classList.toggle('active', e.dataset.tab === name);
  });
  document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
  _('tab-' + name).style.display = 'block';
  if (name === 'dashboard') { loadStats(); loadAccounts(); }
  if (name === 'accounts') loadAccounts();
  if (name === 'logs') loadRequestLogs(true);
  if (name === 'settings') { loadKeys(); loadModels(); }
}

// 导入子标签
document.querySelectorAll('#importTabs .tab').forEach(el => {
  el.addEventListener('click', () => {
    document.querySelectorAll('#importTabs .tab').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('#import-oauth,#import-token,#import-batch').forEach(e => e.classList.remove('active'));
    _('import-' + el.dataset.tab).classList.add('active');
  });
});

// ========== API 请求 ==========
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const res = await fetch(API + path, opts);
  const data = await res.json();
  if (!data.success && data.error) throw new Error(data.error);
  return data;
}

// 用系统默认浏览器打开外部链接（桌面 WebView 内导航不会跳外部浏览器，需走后端）
async function openExternal(url) {
  try {
    await api('GET', '/open-external?url=' + encodeURIComponent(url));
    toast('已在系统浏览器中打开', 'success');
  } catch (e) { toast('打开失败: ' + e.message, 'error'); }
}

// ========== 仪表盘 ==========
async function loadStats() {
  try {
    const d = await api('GET', '/stats');
    const s = d.data;
    _('statTotal').textContent = s.total;
    _('statActive').textContent = s.active;
    _('statCooldown').textContent = s.cooldown;
    _('statExpired').textContent = s.expired;
    _('statPromptTokens').textContent = formatTokenCount(s.promptTokens);
    _('statCompletionTokens').textContent = formatTokenCount(s.completionTokens);
    _('statTotalTokens').textContent = formatTokenCount(s.totalTokens);
    _('statCachedTokens').textContent = formatTokenCount(s.cachedTokens);
    if (s.version) _('settingVersion').value = s.version;
    if (s.strategy) _('settingStrategy').value = s.strategy;
  } catch (e) { /* ignore */ }
}

// ========== 账号管理 ==========
async function loadAccounts() {
  try {
    const d = await api('GET', '/accounts');
    const list = d.data.accounts;
    const tbody = _('accountTableBody');
    const cards = _('accountCards');
    if (!list || list.length === 0) {
      tbody.innerHTML = '<tr><td colspan="10" class="empty">暂无账号，前往 <a href="#" onclick="switchTab(\'import\')" style="color:var(--accent);cursor:pointer">导入账号</a> 页添加</td></tr>';
      cards.innerHTML = '<div class="empty">暂无账号，前往 <a href="#" onclick="switchTab(\'import\')">导入账号</a> 页添加</div>';
      return;
    }
    const sn = { active: '活跃', cooldown: '冷却', expired: '已过期' };
    tbody.innerHTML = list.map(a => {
      const lu = a.lastUsed ? new Date(a.lastUsed).toLocaleString('zh-CN') : '-';
      const cr = a.createdAt ? new Date(a.createdAt).toLocaleString('zh-CN') : '-';
      const statusBadge = a.status === 'cooldown' && a.cooldownUntil
        ? '<span class="status cooldown"><span class="status-dot cooldown"></span>冷却 · ' + formatCooldown(a.cooldownUntil) + '</span>'
        : '<span class="status ' + a.status + '"><span class="status-dot ' + a.status + '"></span>' + (sn[a.status] || a.status) + '</span>';
      return '<tr>' +
        '<td>' + esc(a.email) + '</td>' +
        '<td>' + statusBadge + '</td>' +
        '<td>' + formatNumber(a.usageCount) + '</td>' +
        '<td>' + formatTokenCount(a.promptTokens) + '</td>' +
        '<td>' + formatTokenCount(a.completionTokens) + '</td>' +
        '<td>' + formatTokenCount(a.totalTokens) + '</td>' +
        '<td>' + formatTokenCount(a.cachedTokens) + '</td>' +
        '<td class="mono" style="font-size:11px">' + lu + '</td>' +
        '<td class="mono" style="font-size:11px">' + cr + '</td>' +
        '<td style="white-space:nowrap">' +
          '<button class="btn btn-sm" onclick="testAccount(\'' + a.accountId + '\',this)" title="测试">⚡</button> ' +
          '<button class="btn btn-sm" onclick="resetAccount(\'' + a.accountId + '\')" title="重置">↻</button> ' +
          '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.accountId + '\')" title="删除">✕</button>' +
        '</td></tr>';
    }).join('');
    cards.innerHTML = list.map(a => {
      const lu = a.lastUsed ? new Date(a.lastUsed).toLocaleString('zh-CN') : '从未使用';
      const cardStatus = a.status === 'cooldown' && a.cooldownUntil
        ? '<span class="status cooldown"><span class="status-dot cooldown"></span>冷却 · ' + formatCooldown(a.cooldownUntil) + '</span>'
        : '<span class="status ' + a.status + '"><span class="status-dot ' + a.status + '"></span>' + (sn[a.status] || a.status) + '</span>';
      return '<article class="account-card">' +
        '<div class="account-card-header"><span class="account-email">' + esc(a.email) + '</span>' +
        cardStatus + '</div>' +
        '<div class="account-metrics">' +
          '<div class="account-metric"><span class="account-metric-label">请求</span><span class="account-metric-value">' + formatNumber(a.usageCount) + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">总 Token</span><span class="account-metric-value">' + formatTokenCount(a.totalTokens) + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">缓存</span><span class="account-metric-value">' + formatTokenCount(a.cachedTokens) + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">输入</span><span class="account-metric-value">' + formatTokenCount(a.promptTokens) + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">输出</span><span class="account-metric-value">' + formatTokenCount(a.completionTokens) + '</span></div>' +
        '</div>' +
        '<div class="account-card-footer"><span>最后使用：' + lu + '</span><span class="account-card-actions">' +
          '<button class="btn btn-sm" onclick="testAccount(\'' + a.accountId + '\',this)" title="测试">⚡</button>' +
          '<button class="btn btn-sm" onclick="resetAccount(\'' + a.accountId + '\')" title="重置">↻</button>' +
          '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.accountId + '\')" title="删除">✕</button>' +
        '</span></div></article>';
    }).join('');
  } catch (e) { toast('加载账号失败: ' + e.message, 'error'); }
}

async function testAccount(id, btn) {
  const orig = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span>'; }
  try {
    const d = await api('POST', '/accounts/test', { accountId: id });
    const r = (d.data.results || [])[0];
    if (r && r.ok) {
      const tok = r.inputTokens || r.outputTokens ? ' · 输入 ' + formatTokenCount(r.inputTokens) + ' · 输出 ' + formatTokenCount(r.outputTokens) : '';
      toast('测试成功：' + esc(r.email) + ' · ' + formatDuration(r.durationMs) + tok, 'success');
    } else {
      toast('测试失败：' + esc(r ? r.email : '?') + ' · ' + (r ? r.error : '未知错误'), 'error');
    }
    loadAccounts(); loadStats();
  } catch (e) { toast('测试失败: ' + e.message, 'error'); }
  if (btn) { btn.disabled = false; btn.innerHTML = orig; }
}

async function testAllAccounts(btn) {
  const orig = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span> 测试中...'; }
  toast('正在测试全部账号，请稍候...', 'info');
  try {
    const d = await api('POST', '/accounts/test', {});
    const results = d.data.results || [];
    const ok = results.filter(r => r.ok).length;
    const fail = results.length - ok;
    if (fail === 0) {
      toast('全部测试通过：' + ok + '/' + results.length + ' 个账号正常', 'success');
    } else {
      const failed = results.filter(r => !r.ok).map(r => esc(r.email) + '(' + r.error + ')').join('，');
      toast('测试完成：' + ok + ' 成功 / ' + fail + ' 失败 · ' + failed, 'error');
    }
    loadAccounts(); loadStats();
  } catch (e) { toast('测试失败: ' + e.message, 'error'); }
  if (btn) { btn.disabled = false; btn.innerHTML = orig; }
}

async function deleteAccount(id) {
  if (!confirm('确定删除此账号？')) return;
  try {
    await api('POST', '/accounts/delete', { accountId: id });
    toast('账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function resetAccount(id) {
  if (!confirm('确定重置此账号？将恢复为活跃状态并刷新 Token，保留历史统计。')) return;
  try {
    await api('POST', '/accounts/reset', { accountId: id });
    toast('账号已重置', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('重置失败: ' + e.message, 'error'); }
}

async function deleteAllAccounts() {
  if (!confirm('⚠️ 确定删除所有账号？不可撤销！')) return;
  try {
    await api('POST', '/accounts/delete-all', {});
    toast('全部账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function refreshAllTokens() {
  try {
    await api('POST', '/accounts/refresh-all', {});
    toast('全部 Token 已刷新', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

// ========== OAuth 登录 ==========
async function startOAuth() {
  const btn = _('oauthBtn');
  btn.disabled = true;
  btn.innerHTML = '<span class="loading"></span> 启动中...';
  _('oauthProgress').style.display = 'block';
  _('oauthResult').style.display = 'none';
  _('oauthStatus').textContent = '正在连接 WorkOS...';
  try {
    const d = await api('POST', '/oauth/start');
    const s = d.data;
    _('oauthStatus').textContent = '请在浏览器中打开链接并输入代码';
    const u = _('oauthUrl');
    u.textContent = s.verificationUri;
    u.href = '#';
    u.onclick = async function(e) { e.preventDefault(); await openExternal(s.verificationUri); };
    _('oauthUserCode').textContent = s.userCode;
    const poll = setInterval(async () => {
      try {
        const r = await api('GET', '/oauth/status?sessionId=' + s.sessionId);
        if (r.data.done) {
          clearInterval(poll);
          btn.disabled = false;
          btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M12 5l7 7-7 7"/></svg>开始 OAuth 登录';
          if (r.data.success) {
            _('oauthProgress').style.display = 'none';
            _('oauthResult').innerHTML = '<div style="color:var(--green);font-weight:600">✓ 账号添加成功: ' + esc(r.data.email) + '</div>';
            _('oauthResult').style.display = 'block';
            loadAccounts(); loadStats();
            toast('账号添加成功！', 'success');
          } else {
            _('oauthStatus').textContent = '失败: ' + (r.data.error || '未知错误');
            toast('OAuth 失败', 'error');
          }
        }
      } catch(e) {}
    }, 2000);
  } catch (e) {
    btn.disabled = false;
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M12 5l7 7-7 7"/></svg>开始 OAuth 登录';
    _('oauthStatus').textContent = '错误: ' + e.message;
    toast('OAuth 失败: ' + e.message, 'error');
  }
}

// ========== Token 导入 ==========
async function addByToken() {
  const token = _('tokenInput').value.trim();
  if (!token) { toast('请输入 refreshToken', 'error'); return; }
  const email = _('tokenEmail').value.trim();
  try {
    const d = await api('POST', '/accounts/add', { refreshToken: token, email: email || undefined });
    toast('账号添加成功: ' + (d.data.email || ''), 'success');
    _('tokenInput').value = '';
    _('tokenEmail').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('添加失败: ' + e.message, 'error'); }
}

// ========== 导出账号 ==========
async function exportAccounts() {
  try {
    const res = await fetch(API + '/accounts/export');
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'cline-accounts-export.json';
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
    toast('账号已导出', 'success');
  } catch (e) { toast('导出失败: ' + e.message, 'error'); }
}

// ========== 批量导入 ==========
async function batchImport() {
  const raw = _('batchInput').value.trim();
  if (!raw) { toast('请输入账号数据', 'error'); return; }
  let tokens;
  try { tokens = JSON.parse(raw); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = raw.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入完成', 'success');
    _('batchInput').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
}

async function handleFileImport(event) {
  const file = event.target.files[0];
  if (!file) return;
  const text = await file.text();
  let tokens;
  try { tokens = JSON.parse(text); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = text.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入了 ' + tokens.length + ' 个账号', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
  event.target.value = '';
}

// ========== API 密钥管理 ==========
async function loadKeys() {
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys;
    const el = _('keysList');
    if (!keys || keys.length === 0) {
      el.innerHTML = '<div class="empty-state"><div class="icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg></div>暂无 API 密钥</div>';
      return;
    }
    el.innerHTML = keys.map(k =>
      '<div class="flex" style="margin-bottom:8px">' +
        '<span class="key-display" style="flex:1" onclick="copyText(\'' + k + '\')" title="点击复制">' + esc(k) + '</span>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteKey(\'' + k + '\')">✕</button>' +
      '</div>'
    ).join('');
  } catch (e) { _('keysList').innerHTML = '<div class="empty">加载失败</div>'; }
}

async function generateKey() {
  try {
    const d = await api('POST', '/keys/generate');
    const key = d.data.key;
    _('keyGenResult').innerHTML =
      '<div style="background:var(--green-soft);border:1px solid var(--green);border-radius:var(--radius-sm);padding:14px">' +
        '<div style="color:var(--green);font-weight:600;margin-bottom:8px">✓ 新密钥已生成（点击复制）</div>' +
        '<div class="key-display" onclick="copyText(\'' + key + '\')">' + esc(key) + '</div>' +
      '</div>';
    loadKeys();
    toast('密钥已生成', 'success');
    setTimeout(() => _('keyGenResult').innerHTML = '', 8000);
  } catch (e) { toast('生成失败: ' + e.message, 'error'); }
}

async function deleteKey(key) {
  if (!confirm('确定删除此密钥？')) return;
  try {
    await api('POST', '/keys/delete', { key });
    toast('密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function deleteAllKeys() {
  if (!confirm('确定删除所有 API 密钥？')) return;
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys || [];
    for (const k of keys) await api('POST', '/keys/delete', { key: k });
    toast('全部密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

function copyText(t) {
  navigator.clipboard.writeText(t).then(() => toast('已复制到剪贴板', 'success')).catch(() => {
    const ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta);
    toast('已复制到剪贴板', 'success');
  });
}

// ========== 配置管理 ==========
async function updateConfig() {
  const strategy = _('settingStrategy').value;
  try {
    await api('POST', '/config/update', { strategy });
    toast('策略已更新为: ' + strategy, 'success');
  } catch (e) { toast('更新失败: ' + e.message, 'error'); }
}

function addHeaderRow() {
  const tbody = _('headersTableBody');
  const tr = document.createElement('tr');
  tr.innerHTML =
    '<td><input type="text" class="header-key" placeholder="Header-Name" style="font-size:12px;font-family:ui-monospace,monospace"></td>' +
    '<td><input type="text" class="header-val" placeholder="value" style="font-size:12px;font-family:ui-monospace,monospace"></td>' +
    '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>';
  tbody.appendChild(tr);
}

async function saveHeaders() {
  const tbody = _('headersTableBody');
  const rows = tbody.querySelectorAll('tr');
  const headers = {};
  let hasEmpty = false;
  rows.forEach(tr => {
    const keyInput = tr.querySelector('.header-key');
    const valInput = tr.querySelector('.header-val');
    if (keyInput && valInput) {
      const k = keyInput.value.trim();
      const v = valInput.value.trim();
      if (k) { headers[k] = v; }
      else if (v) { hasEmpty = true; }
    }
  });
  if (hasEmpty) { toast('存在有值无键的行，已忽略', 'info'); }
  try {
    const d = await api('POST', '/config/update', { headers });
    toast('请求头已保存', 'success');
    _('headerSaveResult').innerHTML =
      '<div style="color:var(--green);font-size:13px">✓ 已保存 ' + Object.keys(d.data.headers).length + ' 个请求头</div>';
    setTimeout(() => _('headerSaveResult').innerHTML = '', 5000);
    loadConfig();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 模型列表 ==========
async function loadModels() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    _('modelsList').innerHTML = models.map(m =>
      '<span class="model-tag ' + (m.cost || 'free') + '">' + esc(m.id) + '</span>'
    ).join('') || '<div class="empty">暂无模型</div>';
  } catch (e) { _('modelsList').textContent = '加载失败'; }
}

// ========== 配置加载 ==========
async function loadConfig() {
  try {
    const d = await api('GET', '/config');
    const c = d.data;
    if (c.address) _('settingAddr').value = c.address;
    if (c.strategy) _('settingStrategy').value = c.strategy;
    if (c.version) _('settingVersion').value = c.version;
    if (c.poolPath) _('settingPoolPath').value = c.poolPath;
    if (c.defaultModel) _('settingDefModel').value = c.defaultModel;
    if (c.headers) {
      const tbody = _('headersTableBody');
      tbody.innerHTML = Object.entries(c.headers).map(([k, v]) =>
        '<tr>' +
          '<td><input type="text" class="header-key" value="' + esc(k) + '" style="font-size:12px;font-family:ui-monospace,monospace;width:100%"></td>' +
          '<td><input type="text" class="header-val" value="' + esc(v) + '" style="font-size:12px;font-family:ui-monospace,monospace;width:100%"></td>' +
          '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>' +
        '</tr>'
      ).join('');
    }
  } catch (e) { /* ignore */ }
}

// ========== 请求日志 ==========
let logCursor = '';
let logHasMore = false;

const formatDuration = ms => {
  if (!ms || ms <= 0) return '-';
  if (ms < 1000) return ms + 'ms';
  return (ms / 1000).toFixed(1) + 's';
};
const formatTPS = v => (!v || v <= 0) ? '-' : v.toFixed(1);

async function loadRequestLogs(reset) {
  if (reset) logCursor = '';
  const cursor = logCursor;
  try {
    const path = '/request-logs?limit=50' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : '');
    const d = await api('GET', path);
    const page = d.data;
    const items = page.items || [];
    logHasMore = !!page.hasMore;
    logCursor = page.nextCursor || '';
    _('logLoadMore').style.display = logHasMore ? 'block' : 'none';

    const tbody = _('logTableBody');
    const cards = _('logCards');
    if (reset && (!items || items.length === 0)) {
      tbody.innerHTML = '<tr><td colspan="12" class="empty">暂无请求日志</td></tr>';
      cards.innerHTML = '<div class="empty">暂无请求日志</div>';
      return;
    }

    const renderRow = l => {
      const t = l.startedAt ? new Date(l.startedAt).toLocaleString('zh-CN') : '-';
      const st = l.completed
        ? '<span class="log-status ok">完成</span>'
        : '<span class="log-status fail" title="' + esc(l.error || '') + '">失败</span>';
      const tk = l.usageAvailable
        ? formatTokenCount(l.inputTokens) + '</td><td>' + formatTokenCount(l.outputTokens) + '</td><td>' + formatTokenCount(l.cachedTokens) + '</td><td>' + formatTokenCount(l.totalTokens)
        : '-</td><td>-</td><td>-</td><td>-';
      return '<tr>' +
        '<td class="mono" style="font-size:11px">' + t + '</td>' +
        '<td>' + esc(l.accountEmail || '-') + '</td>' +
        '<td>' + esc(l.protocol || '-') + '</td>' +
        '<td class="mono" style="font-size:11px">' + esc(l.model || '-') + '</td>' +
        '<td>' + tk + '</td>' +
        '<td>' + formatDuration(l.durationMs) + '</td>' +
        '<td>' + (l.ttftMs ? formatDuration(l.ttftMs) : '-') + '</td>' +
        '<td>' + formatTPS(l.outputTokensPerSecond) + '</td>' +
        '<td>' + st + '</td></tr>';
    };
    const renderCard = l => {
      const t = l.startedAt ? new Date(l.startedAt).toLocaleString('zh-CN') : '-';
      const st = l.completed ? '完成' : '失败';
      const tk = l.usageAvailable
        ? '输入 ' + formatTokenCount(l.inputTokens) + ' · 输出 ' + formatTokenCount(l.outputTokens) + ' · 缓存 ' + formatTokenCount(l.cachedTokens) + ' · 总 ' + formatTokenCount(l.totalTokens)
        : 'Token 未知';
      return '<article class="account-card">' +
        '<div class="account-card-header"><span class="account-email">' + esc(l.accountEmail || '-') + '</span><span class="log-status ' + (l.completed ? 'ok' : 'fail') + '">' + st + '</span></div>' +
        '<div class="account-metrics">' +
          '<div class="account-metric"><span class="account-metric-label">协议</span><span class="account-metric-value">' + esc(l.protocol || '-') + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">耗时</span><span class="account-metric-value">' + formatDuration(l.durationMs) + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">TTFT</span><span class="account-metric-value">' + (l.ttftMs ? formatDuration(l.ttftMs) : '-') + '</span></div>' +
          '<div class="account-metric"><span class="account-metric-label">tok/s</span><span class="account-metric-value">' + formatTPS(l.outputTokensPerSecond) + '</span></div>' +
        '</div>' +
        '<div style="font-size:12px;color:var(--text2);margin-bottom:8px">' + tk + '</div>' +
        '<div class="account-card-footer"><span class="mono" style="font-size:11px">' + t + '</span><span class="mono" style="font-size:11px">' + esc(l.model || '-') + '</span></div>' +
      '</article>';
    };

    if (reset) {
      tbody.innerHTML = items.map(renderRow).join('');
      cards.innerHTML = items.map(renderCard).join('');
    } else {
      tbody.insertAdjacentHTML('beforeend', items.map(renderRow).join(''));
      cards.insertAdjacentHTML('beforeend', items.map(renderCard).join(''));
    }
  } catch (e) { toast('加载日志失败: ' + e.message, 'error'); }
}

// ========== 初始化 ==========
loadStats();
loadAccounts();
loadKeys();
loadModels();
loadConfig();
setInterval(() => { loadStats(); }, 10000);
</script>
</body>
</html>`

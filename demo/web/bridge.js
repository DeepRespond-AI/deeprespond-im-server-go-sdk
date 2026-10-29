// WebSocket 请求与回复；由入口注入推送和断线处理。

let ws = null, reqSeq = 1;
const pendingReplies = new Map();

export function bridge(action, params = {}) {
  return new Promise((resolve, reject) => {
    if (!ws || ws.readyState !== 1) return reject(new Error("连接未就绪"));
    const id = reqSeq++;
    pendingReplies.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, action, params }));
    setTimeout(() => {
      if (pendingReplies.has(id)) {
        pendingReplies.delete(id);
        reject(new Error(`「${action}」请求超时`));
      }
    }, 15000);
  });
}

export function connectBridge(onPush, onClose) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.onmessage = (ev) => {
    let f; try { f = JSON.parse(ev.data); } catch { return; }
    if (f.type === "reply") {
      const p = pendingReplies.get(f.id);
      if (!p) return;
      pendingReplies.delete(f.id);
      if (f.ok) p.resolve(f.data); else p.reject(new Error(f.error || "请求失败"));
    } else if (f.type === "push") {
      onPush(f.event, f.data);
    }
  };
  ws.onclose = () => {
    for (const pending of pendingReplies.values()) pending.reject(new Error("连接已断开"));
    pendingReplies.clear();
    onClose();
  };
}

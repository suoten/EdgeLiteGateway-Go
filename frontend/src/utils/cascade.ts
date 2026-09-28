// 后端 /system/cascade/* 目前是占位实现：返回固定字面量（拓扑恒为 {nodes:[],edges:[],root:null}，
// 邻居恒为 []），从不查询已经在跑的 engine.CascadeManager。
// 前端必须区分“这个网关没有邻居”和“后端根本没给出拓扑数据”——前者是一个拓扑结论，
// 后者才是当前的实情；把后者渲染成前者会让用户以为级联链路已建立且正常。
export function cascadeDataMissing(data: unknown): boolean {
  if (!data || typeof data !== 'object') return true
  const d = data as Record<string, unknown>
  if (typeof d.status === 'string' && d.status !== '') return false
  if (typeof d.role === 'string' && d.role !== '') return false
  if (typeof d.local_id === 'string' && d.local_id !== '') return false
  // The view renders peers/children, so an empty array is still a real answer:
  // it means "standalone, nothing attached", which is a topology conclusion.
  for (const key of ['peers', 'children', 'neighbors']) {
    if (Array.isArray(d[key])) return false
  }
  // A graph-shaped payload counts as real only once it names something: the live
  // placeholder answers {nodes: [], edges: [], root: null}, and an engine that is
  // actually wired up would at least list this gateway as a node.
  for (const key of ['nodes', 'edges']) {
    const v = d[key]
    if (Array.isArray(v) && v.length > 0) return false
  }
  if (typeof d.root === 'string' && d.root !== '') return false
  return true
}

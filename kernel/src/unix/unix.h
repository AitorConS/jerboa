typedef struct kernel_heaps *kernel_heaps;
typedef struct unix_heaps *unix_heaps;
typedef struct process *process;
typedef struct thread *thread;

process init_unix(kernel_heaps kh, tuple root, filesystem fs);
process create_process(unix_heaps uh, tuple root, filesystem fs);
void process_get_cwd(process p, filesystem *cwd_fs, inode *cwd);
thread create_thread(process p, u64 tid);
void exec_elf(process kp, string program_path, status_handler complete);
void unix_shutdown(void);

/* env_inject_from_fw_cfg merges QEMU fw_cfg "opt/uni/env" entries into
 * root[environment] before exec_elf reads it. No-op if device/file absent. */
void env_inject_from_fw_cfg(tuple root);

/* net_inject_from_fw_cfg merges QEMU fw_cfg "opt/uni/network" static IP
 * configuration into the root tuple before init_network_iface. No-op if absent. */
void net_inject_from_fw_cfg(tuple root);

/* mounts_inject_from_fw_cfg merges QEMU fw_cfg "opt/uni/mounts" volume mount
 * points ("LABEL:/path" per line) into root[mounts]. Returns true if any entry
 * was added, so the caller re-applies storage_set_mountpoints. No-op if absent. */
boolean mounts_inject_from_fw_cfg(tuple root);

void program_set_perms(tuple root, tuple prog);

void dump_mem_stats(buffer b);

void coredump_set_limit(u64 s);
u64 coredump_get_limit(void);

timestamp proc_utime(process p);
timestamp proc_stime(process p);

timestamp thread_utime(thread t);
timestamp thread_stime(thread t);

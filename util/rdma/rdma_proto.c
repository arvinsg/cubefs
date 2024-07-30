#include "rdma_proto.h"

int WQ_DEPTH = 32;
int WQ_SG_DEPTH = 2;
int MIN_CQE_NUM = 1024;
int CONN_DATA_SIZE = 128*1024*32;

struct rdma_pool *rdma_pool = NULL;
struct rdma_env_config *rdma_env_config = NULL;
FILE *debug_fp = NULL;
FILE *error_fp = NULL;
struct net_env_st *g_net_env = NULL;


uint64_t allocate_nd(int type) {
    int id_index = ID_GEN_CTRL;
    union conn_nd_union id;

    id_index += 1;
    id.nd_.worker_id = __sync_fetch_and_add((g_net_env->id_gen + id_index), 1) & 0xFF;
    id.nd_.type = type & 0xFF;
    id.nd_.m1 = 'c';
    id.nd_.m2 = 'b';
    id.nd_.id = __sync_fetch_and_add((g_net_env->id_gen + ID_GEN_MAX -1), 1);
    return id.nd;
}

void cbrdma_parse_nd(uint64_t nd, int *id, int * worker_id, int * is_server, int * is_active) {
    *id = (nd & 0xFFFFFFFF);
    *worker_id = ((nd >> 32) & 0xFF);
    uint8_t type  = (((nd >> 32) & 0xFF00) >> 8);
    *is_server = type & 0x80;
    *is_active = type & 0x40;
}

struct rdma_env_config* get_rdma_env_config() {
    rdma_env_config = (struct rdma_env_config*)malloc(sizeof(struct rdma_env_config));
    memset(rdma_env_config, 0, sizeof(struct rdma_env_config));
    rdma_env_config->mem_block_num = 4 * 8 * 1024;
    rdma_env_config->mem_block_size = 128 * 1024;
    rdma_env_config->mem_pool_level = 15;
    rdma_env_config->conn_data_size = 128 * 1024 * 32;
    rdma_env_config->wq_depth = 32;
    rdma_env_config->min_cqe_num = 1024;
    rdma_env_config->enable_rdma_log = 0;
    rdma_env_config->rdma_log_dir = malloc(256 * sizeof(char));
    strcpy(rdma_env_config->rdma_log_dir, "/");
    rdma_env_config->worker_num = 4;
    return rdma_env_config;
}

int init_worker(worker *worker, event_callback cb, int index) {
    int ret = 0;
    char str[20];
    cpu_set_t cpuset;

    worker->pd = g_net_env->pd;
    //log_debug("ibv_alloc_pd:%p", worker->pd);
    worker->comp_channel = ibv_create_comp_channel(g_net_env->ctx);
    if (worker->comp_channel == NULL) {
        log_error("worker(%p) ibv create comp channel failed", worker);
        return C_ERR;
    }
    //log_debug("ibv_create_comp_channel:%p",worker->comp_channel);
    worker->cq = ibv_create_cq(g_net_env->ctx, MIN_CQE_NUM, NULL, worker->comp_channel, 0);
    if (worker->cq == NULL) {
        //return assert,ignore resource free
        log_error("worker(%p) create cq failed, errno:%d", worker, errno);
        goto err_destroy_compchannel;
    }
    //log_debug("ibv_create_cq:%p", worker->cq);
    ibv_req_notify_cq(worker->cq, 0);

    ret = pthread_spin_init(&(worker->lock), PTHREAD_PROCESS_SHARED);
    if (ret != 0) {
        log_error("worker(%p) init spin lock failed, err:%d", worker, ret);
        goto err_destroy_cq;
    }
    ret = pthread_spin_init(&(worker->nd_map_lock), PTHREAD_PROCESS_SHARED);
    if (ret != 0) {
        log_error("worker(%p) init spin nd map lock failed, err:%d", worker, ret);
        goto err_destroy_workerlock;
    }
    worker->nd_map = hashmap_create();
    worker->closing_nd_map = hashmap_create();
    //list_head_init(&worker->conn_list);
    worker->conn_list = InitQueue();
    if (worker->conn_list == NULL) {
        log_error("worker(%p) init conn list failed", worker);
        goto err_destroy_map;
    }
    worker->w_pid = 0;

    pthread_create(&worker->cq_poller_thread, NULL, cb, worker);
    sprintf(str, "cq_worker:%d", index);
    pthread_setname_np(worker->cq_poller_thread, str);
    //__CPU_ZERO_S(sizeof(cpu_set_t), &cpuset);
    //__CPU_SET_S(index, sizeof(cpu_set_t), &cpuset);
    //pthread_setaffinity_np(worker->cq_poller_thread, sizeof(cpu_set_t), &cpuset);

    return C_OK;
err_destroy_map:
    hashmap_destroy(worker->closing_nd_map);
    hashmap_destroy(worker->nd_map);
    pthread_spin_destroy(&worker->nd_map_lock);
err_destroy_workerlock:
    pthread_spin_destroy(&worker->lock);
err_destroy_cq:
    ibv_destroy_cq(worker->cq);
err_destroy_compchannel:
    ibv_destroy_comp_channel(worker->comp_channel);
    return C_ERR;
}

void destroy_worker(worker *worker) {
    worker->close = 1;
    pthread_join(worker->cq_poller_thread, NULL);
    worker->w_pid = 0;

    if (worker->conn_list != NULL) {
        DestroyQueue(worker->conn_list);
        worker->conn_list = NULL;
    }
    if (worker->closing_nd_map != NULL) {
        hashmap_destroy(worker->closing_nd_map);
        worker->closing_nd_map = NULL;
    }
    if (worker->nd_map != NULL) {
        hashmap_destroy(worker->nd_map);
        worker->nd_map = NULL;
    }
    pthread_spin_destroy(&worker->nd_map_lock);
    pthread_spin_destroy(&worker->lock);

    if (worker->cq != NULL) {
        log_debug("worker(%p) ibv_destroy_cq:%p", worker, worker->cq);
        ibv_destroy_cq(worker->cq);
        worker->cq = NULL;
    }
    if(worker->comp_channel != NULL) {
        log_debug("worker(%p) ibv_destroy_comp_channel:%p", worker, worker->comp_channel);
        ibv_destroy_comp_channel(worker->comp_channel);
        worker->comp_channel = NULL;
    }

    worker->pd = NULL;
}

void destroy_rdma_env() {
    if (g_net_env != NULL) {
        for (int i = 0; i < g_net_env->worker_num; i++) {
            destroy_worker(g_net_env->worker + i);
        }

        if (g_net_env->event_channel != NULL) {
            rdma_destroy_event_channel(g_net_env->event_channel);
            g_net_env->event_channel = NULL;
        }

        g_net_env->close = 1;
        pthread_join(g_net_env->cm_event_loop_thread, NULL);

        if (g_net_env->all_devs != NULL) {
            rdma_free_devices(g_net_env->all_devs);
            g_net_env->all_devs = NULL;
        }

        pthread_spin_destroy(&g_net_env->server_lock);

        hashmap_destroy(g_net_env->server_map);

        free(g_net_env);
        g_net_env = NULL;
    }

    if (rdma_pool != NULL) {
        if(rdma_pool->memory_pool != NULL) {
            close_memory_pool(rdma_pool->memory_pool);
        }
        free(rdma_pool);
    }
    if (rdma_env_config != NULL) {
        free(rdma_env_config->rdma_log_dir);
        free(rdma_env_config);
    }

    if (debug_fp != NULL) {
        fclose(debug_fp);
    }
    if (error_fp != NULL) {
        fclose(error_fp);
    }
}

int init_rdma_env(struct rdma_env_config* config) {
    if(config == NULL) {
        return C_ERR;
    }

    rdma_env_config = config;

    if (rdma_env_config->enable_rdma_log == 1) {
        log_set_level(0);
        log_set_quiet(0);
        char* debug_name = "rdma_debug.log";
        char* error_name = "rdma_error.log";
        char* debug_path = (char*)malloc(strlen(rdma_env_config->rdma_log_dir) + strlen(debug_name));
        char* error_path = (char*)malloc(strlen(rdma_env_config->rdma_log_dir) + strlen(error_name));
        strcpy(debug_path, rdma_env_config->rdma_log_dir);
        strcat(debug_path, debug_name);
        strcpy(error_path, rdma_env_config->rdma_log_dir);
        strcat(error_path, error_name);
        debug_fp = fopen(debug_path, "ab");
        if (debug_fp == NULL) {
            goto err_free_config;
        }
        log_add_fp(debug_fp, LOG_DEBUG);
        error_fp = fopen(error_path, "ab");
        if (error_fp == NULL) {
            goto err_close_debug_fp;
        }
        log_add_fp(error_fp, LOG_ERROR);
    } else {
        log_set_quiet(1);
    }

    int len = sizeof(struct net_env_st) + config->worker_num * sizeof(worker);
    g_net_env = (struct net_env_st*)malloc(len);
    if (g_net_env == NULL) {
        log_error("init env failed: no enough memory");
        goto err_close_error_fp;
    }
    g_net_env->worker_num = config->worker_num;//32
    g_net_env->server_map = hashmap_create();

    if (pthread_spin_init(&(g_net_env->server_lock), PTHREAD_PROCESS_SHARED) != 0) {
        log_error("init g_net_env->server_lock spin lock failed");
        goto err_free_gnetenv;
    }

    g_net_env->all_devs = rdma_get_devices(&g_net_env->ib_dev_cnt);
    if (g_net_env->all_devs == NULL) {
        log_error("init env failed: get rdma devices failed");
        goto err_destroy_spinlock;
    }
    log_debug("rdma_get_devices find ib_dev_cnt:%d", g_net_env->ib_dev_cnt);

    if (g_net_env->ib_dev_cnt > 0) {
        g_net_env->ctx = g_net_env->all_devs[0];
    } else {
        log_error("can not find rdma dev");
        goto err_free_devices;
    }

    g_net_env->event_channel = rdma_create_event_channel();
    g_net_env->pd = ibv_alloc_pd(g_net_env->ctx);
    if (g_net_env->pd == NULL) {
        log_error("alloc pd failed, errno:%d", errno);
        goto err_destroy_eventchannel;
    }
    //log_debug("g net env alloc pd:%p",g_net_env->pd);

    pthread_create(&g_net_env->cm_event_loop_thread, NULL, cm_thread, g_net_env);
    pthread_setname_np(g_net_env->cm_event_loop_thread, "cm_worker");
    int index;
    for (index = 0; index < g_net_env->worker_num; index++) {
        log_debug("init worker(%d-%p)", index, g_net_env->worker + index);
        g_net_env->worker[index].id = index;
        if(init_worker(g_net_env->worker + index, cq_thread, index) == C_ERR) {
            log_error("init env failed: init worker(%d-%p) failed", index, g_net_env->worker + index);
            goto err_destroy_worker;
        }
    }
    WQ_DEPTH = rdma_env_config->wq_depth;
    MIN_CQE_NUM = rdma_env_config->min_cqe_num;
    CONN_DATA_SIZE = rdma_env_config->conn_data_size;
    rdma_pool = (struct rdma_pool*)malloc(sizeof(struct rdma_pool));
    if (rdma_pool == NULL) {
        log_error("malloc rdma pool failed");
        goto err_destroy_worker;
    }
    memset(rdma_pool, 0, sizeof(struct rdma_pool));
    rdma_pool->memory_pool = init_memory_pool(rdma_env_config->mem_block_num, rdma_env_config->mem_block_size, rdma_env_config->mem_pool_level, g_net_env->pd);
    if(rdma_pool->memory_pool == NULL) {
        log_error("init rdma memory pool failed");
        goto err_free_rdmapool;
    }
    return C_OK;
err_free_rdmapool:
    free(rdma_pool);
err_destroy_worker:
    ibv_dealloc_pd(g_net_env->pd);
    for (int i = 0; i < index; i++) {
        destroy_worker(g_net_env->worker + i);
    }
err_destroy_eventchannel:
    rdma_destroy_event_channel(g_net_env->event_channel);
err_free_devices:
    rdma_free_devices(g_net_env->all_devs);
err_destroy_spinlock:
    pthread_spin_destroy(&g_net_env->server_lock);
err_free_gnetenv:
    hashmap_destroy(g_net_env->server_map);
    free(g_net_env);
err_close_error_fp:
    fclose(error_fp);
err_close_debug_fp:
    fclose(debug_fp);
err_free_config:
    free(rdma_env_config);
    return C_ERR;
}

/*
void conn_add_ref(connection* conn) {
    pthread_spin_lock(&conn->ref_lock);
    int old_ref = conn->ref;
    if (conn->ref > 0) {
        conn->ref++;
    };
    pthread_spin_unlock(&conn->ref_lock);
    log_debug("conn(%lu-%p) ref: %d-->%d", conn->nd, conn, old_ref, conn->ref);
    return;
}

void conn_del_ref(connection* conn) {
    pthread_spin_lock(&conn->ref_lock);
    int old_ref = conn->ref;
    if (conn->ref > 0) {
        conn->ref--;
    };
    pthread_spin_unlock(&conn->ref_lock);
    log_debug("conn(%lu-%p) ref: %d-->%d", conn->nd, conn, old_ref, conn->ref);
    return;
}

int conn_get_ref(connection* conn) {
    pthread_spin_lock(&conn->ref_lock);
    int ret = conn->ref;
    pthread_spin_unlock(&conn->ref_lock);
    return ret;
}
*/

void set_conn_state(connection* conn, int state) {
    pthread_spin_lock(&conn->spin_lock);
    int old_state = conn->state;
    conn->state = state;
    pthread_spin_unlock(&conn->spin_lock);
    log_debug("conn(%lu-%p) state: %d-->%d", conn->nd, conn, old_state, state);
    return;
}

int get_conn_state(connection* conn) {
    pthread_spin_lock(&conn->spin_lock);
    int state = conn->state;
    pthread_spin_unlock(&conn->spin_lock);
    return state;
}

worker* get_worker_by_nd(uint64_t nd) {
    int worker_id = ((nd) >>32) % g_net_env->worker_num;//CONN_ID_BIT_LEN
    log_debug("get worker by nd: worker_id:%d",worker_id);
    return g_net_env->worker + worker_id;
}

int add_conn_to_worker(connection * conn, worker * worker, khash_t(map) *hmap) {
    int ret = 0;
    pthread_spin_lock(&worker->nd_map_lock);
    ret = hashmap_put(hmap, conn->nd, (uint64_t)conn);
    pthread_spin_unlock(&worker->nd_map_lock);
    log_debug("add conn(%p nd:%d) from worker(%p) nd_map(%p)",conn,conn->nd,worker,worker->nd_map);
    return ret >= 0;
}

int del_conn_from_worker(uint64_t nd, worker * worker, khash_t(map) *hmap) {
    int ret = 0;
    pthread_spin_lock(&worker->nd_map_lock);
    ret = hashmap_del(hmap, nd);
    pthread_spin_unlock(&worker->nd_map_lock);
    log_debug("del conn(nd:%d) from worker(%p) nd_map(%p)",nd,worker,worker->nd_map);
    return ret >= 0;
}

void get_worker_and_connect_by_nd(uint64_t nd, worker ** worker, connection** conn) {
    *worker = get_worker_by_nd(nd);
    pthread_spin_lock(&(*worker)->nd_map_lock);
    *conn = (connection*)hashmap_get((*worker)->nd_map, nd);
    pthread_spin_unlock(&(*worker)->nd_map_lock);
}

int add_server_to_env(struct rdma_listener *server, khash_t(map) *hmap) {
    int ret = 0;
    pthread_spin_lock(&g_net_env->server_lock);
    ret = hashmap_put(hmap, server->nd, (uint64_t)server);
    pthread_spin_unlock(&g_net_env->server_lock);
    return ret >= 0;
}

int del_server_from_env(struct rdma_listener *server) {
    int ret = 0;
    pthread_spin_lock(&g_net_env->server_lock);
    ret = hashmap_del(g_net_env->server_map, server->nd);
    pthread_spin_unlock(&g_net_env->server_lock);
    return ret >= 0;
}

inline int open_event_fd() {
    return eventfd(0, EFD_SEMAPHORE);
}

inline int wait_event(int fd) {
    uint64_t value = 0;
    return read(fd, &value, 8);
}

inline int notify_event(int fd, int flag) {
	if (flag == 0) {
		uint64_t value = 1;
        return write(fd, &value, 8);
	} else {
		close(fd);
		return 0;
	}
}
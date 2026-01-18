import axios from 'axios';
import { jwtDecode } from 'jwt-decode';

// In production (docker), use relative URLs (nginx will proxy)
// In development, use localhost:5000
const API_BASE_URL = import.meta.env.VITE_API_URL || (import.meta.env.PROD ? '' : 'http://localhost:5000');

const api = axios.create({
  baseURL: API_BASE_URL,
  headers: {
    'Content-Type': 'application/json',
  },
});

// Add token to requests
api.interceptors.request.use(
  (config) => {
    const token = localStorage.getItem('token');
    if (token) {
      config.headers.Authorization = `Bearer ${token}`;
    }
    return config;
  },
  (error) => {
    return Promise.reject(error);
  }
);

// Handle auth errors
api.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      localStorage.removeItem('token');
      window.location.href = '/login';
    }
    return Promise.reject(error);
  }
);

export const authAPI = {
  login: async (username, password) => {
    const response = await api.post('/api/auth/login', { username, password });
    if (response.data.token) {
      localStorage.setItem('token', response.data.token);
    }
    return response.data;
  },
  logout: async () => {
    await api.post('/api/auth/logout');
    localStorage.removeItem('token');
  },
  me: async () => {
    const response = await api.get('/api/auth/me');
    return response.data;
  },
};

export const repositoriesAPI = {
  getAll: async () => {
    const response = await api.get('/api/repositories');
    return response.data;
  },
  create: async (name) => {
    const response = await api.post('/api/repositories', { name });
    return response.data;
  },
  delete: async (repoName) => {
    const response = await api.delete(`/api/repositories/${encodeURIComponent(repoName)}`);
    return response.data;
  },
  deleteTag: async (repoName, tagName) => {
    const response = await api.delete(`/api/repositories/${encodeURIComponent(repoName)}/tags/${encodeURIComponent(tagName)}`);
    return response.data;
  },
};

export const dashboardAPI = {
  get: async () => {
    const response = await api.get('/api/dashboard');
    return response.data;
  },
};

export const ftpAPI = {
  getUsage: async () => {
    try {
      const response = await api.get('/api/ftp/usage');
      // Check if response has error field
      if (response.data && response.data.error) {
        throw new Error(response.data.error);
      }
      return response.data;
    } catch (error) {
      // Re-throw with more context
      if (error.response) {
        // Server responded with error status
        const errorMsg = error.response.data?.error || error.response.statusText || 'Unknown error';
        throw new Error(`FTP Usage API error: ${errorMsg} (Status: ${error.response.status})`);
      } else if (error.request) {
        // Request made but no response
        throw new Error('FTP Usage API: No response from server');
      } else {
        // Something else happened
        throw error;
      }
    }
  },
};

export const groupsAPI = {
  getAll: async () => {
    const response = await api.get('/api/groups');
    return response.data;
  },
  create: async (name) => {
    const response = await api.post('/api/groups', { name });
    return response.data;
  },
  getRepositories: async (groupName) => {
    const response = await api.get(`/api/groups/${encodeURIComponent(groupName)}/repositories`);
    return response.data;
  },
  getTags: async (groupName, repoName) => {
    const response = await api.get(`/api/groups/${encodeURIComponent(groupName)}/repositories/${encodeURIComponent(repoName)}/tags`);
    return response.data;
  },
};

export const isAuthenticated = () => {
  const token = localStorage.getItem('token');
  if (!token) return false;
  
  try {
    const decoded = jwtDecode(token);
    const now = Date.now() / 1000;
    return decoded.exp > now;
  } catch {
    return false;
  }
};

export default api;


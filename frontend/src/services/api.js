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

